package gitops

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Identity struct{ Name, Email string }

func (i Identity) validate() error {
	if i.Name == "" || i.Email == "" || strings.ContainsAny(i.Name+i.Email, "<>\n\r") {
		return fmt.Errorf("gitops: invalid identity %q <%s>", i.Name, i.Email)
	}
	return nil
}

// Change is one file operation relative to a base tree. With Tree set, Path is
// a directory that is replaced by that tree object.
type Change struct {
	Path    string
	Content []byte
	Delete  bool
	Tree    string
}

// Trailer is a "Key: value" line at the end of a commit message
// (OVC-User, OVC-Sync, OVC-Hydrate; spec §9.4, §9.7, §9.8).
type Trailer struct{ Key, Value string }

type CommitOpts struct {
	Author, Committer Identity
	Message           string
	Trailers          []Trailer
	When              time.Time // zero = now
}

func (o CommitOpts) full() (string, error) {
	if err := o.Author.validate(); err != nil {
		return "", err
	}
	if err := o.Committer.validate(); err != nil {
		return "", err
	}
	if strings.TrimSpace(o.Message) == "" {
		return "", fmt.Errorf("gitops: empty commit message")
	}
	msg := strings.TrimRight(o.Message, "\n")
	if len(o.Trailers) > 0 {
		msg += "\n"
		for _, t := range o.Trailers {
			if t.Key == "" || strings.ContainsAny(t.Key+t.Value, "\n\r") || strings.Contains(t.Key, ":") {
				return "", fmt.Errorf("gitops: invalid trailer %q", t.Key)
			}
			msg += "\n" + t.Key + ": " + t.Value
		}
	}
	return msg + "\n", nil
}

func (o CommitOpts) when() time.Time {
	if o.When.IsZero() {
		return time.Now()
	}
	return o.When
}

func tmpRef() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "refs/ovc/tmp/" + hex.EncodeToString(b)
}

// quote writes a path for the fast-import stream (always C-quoted).
func quote(p string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < 0x20 || c == 0x7f:
			fmt.Fprintf(&b, "\\%03o", c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// makeCommit builds one commit on top of parent ("" = root commit) applying
// changes, and returns its SHA without moving any branch. merge adds a second
// parent (a merge commit whose tree is parent's tree plus changes).
func (r *Repo) makeCommit(ctx context.Context, parent, merge string, changes []Change, o CommitOpts) (string, error) {
	msg, err := o.full()
	if err != nil {
		return "", err
	}
	for _, c := range changes {
		if err := ValidatePath(c.Path); err != nil {
			return "", err
		}
	}
	ref := tmpRef()
	ts := strconv.FormatInt(o.when().Unix(), 10) + " +0000"
	var s bytes.Buffer
	fmt.Fprintf(&s, "commit %s\n", ref)
	fmt.Fprintf(&s, "author %s <%s> %s\n", o.Author.Name, o.Author.Email, ts)
	fmt.Fprintf(&s, "committer %s <%s> %s\n", o.Committer.Name, o.Committer.Email, ts)
	fmt.Fprintf(&s, "data %d\n%s\n", len(msg), msg)
	if parent != "" {
		fmt.Fprintf(&s, "from %s\n", parent)
	}
	if merge != "" {
		fmt.Fprintf(&s, "merge %s\n", merge)
	}
	for _, c := range changes {
		if c.Tree != "" {
			fmt.Fprintf(&s, "M 040000 %s %s\n", c.Tree, quote(c.Path))
			continue
		}
		if c.Delete {
			fmt.Fprintf(&s, "D %s\n", quote(c.Path))
			continue
		}
		fmt.Fprintf(&s, "M 100644 inline %s\ndata %d\n", quote(c.Path), len(c.Content))
		s.Write(c.Content)
		s.WriteByte('\n')
	}
	if _, err := r.git(ctx, &s, nil, "fast-import", "--quiet"); err != nil {
		return "", err
	}
	out, err := r.git(ctx, nil, nil, "rev-parse", ref)
	if err != nil {
		return "", err
	}
	r.git(ctx, nil, nil, "update-ref", "-d", ref)
	return strings.TrimSpace(string(out)), nil
}

// CreateBranch makes a new root branch holding exactly files (spec §9.1 step 5:
// the baseline of a schema). It fails with ErrExists if the branch exists.
func (r *Repo) CreateBranch(ctx context.Context, branch string, files map[string][]byte, o CommitOpts) (string, error) {
	if _, err := r.Head(ctx, branch); err == nil {
		return "", fmt.Errorf("%w: %s", ErrExists, branch)
	}
	changes := make([]Change, 0, len(files))
	for p, c := range files {
		changes = append(changes, Change{Path: p, Content: c})
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	sha, err := r.makeCommit(ctx, "", "", changes, o)
	if err != nil {
		return "", err
	}
	if err := r.setRef(ctx, branch, sha, strings.Repeat("0", 40)); err != nil {
		return "", fmt.Errorf("%w: %s", ErrExists, branch)
	}
	return sha, nil
}

// BranchFrom creates branch pointing at ref (a branch, tag or SHA), e.g. the
// prd branch from the same baseline as dev (spec §5).
func (r *Repo) BranchFrom(ctx context.Context, branch, ref string) (string, error) {
	out, err := r.git(ctx, nil, nil, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("gitops: unknown ref %q", ref)
	}
	sha := strings.TrimSpace(string(out))
	if err := r.setRef(ctx, branch, sha, strings.Repeat("0", 40)); err != nil {
		return "", fmt.Errorf("%w: %s", ErrExists, branch)
	}
	return sha, nil
}

// setRef moves refs/heads/<branch> to sha only if it currently is old
// (all zeros = must not exist). This is the compare-and-swap that makes
// concurrent writers safe.
func (r *Repo) setRef(ctx context.Context, branch, sha, old string) error {
	_, err := r.git(ctx, nil, nil, "update-ref", "refs/heads/"+branch, sha, old)
	return err
}

// Commit applies changes on top of the branch head and moves the branch. When
// expect is not empty it must equal the current head, otherwise ErrStale.
func (r *Repo) Commit(ctx context.Context, branch, expect string, changes []Change, o CommitOpts) (string, error) {
	head, err := r.Head(ctx, branch)
	if err != nil {
		return "", err
	}
	if expect != "" && head != expect {
		return "", fmt.Errorf("%w: %s is at %s, expected %s", ErrStale, branch, short(head), short(expect))
	}
	sha, err := r.makeCommit(ctx, head, "", changes, o)
	if err != nil {
		return "", err
	}
	if err := r.setRef(ctx, branch, sha, head); err != nil {
		return "", fmt.Errorf("%w: %s moved while committing", ErrStale, branch)
	}
	return sha, nil
}

// CommitTree records an already merged tree as a new commit on branch (parent
// = current head), used after a clean three-way merge (spec §9.4 step 6).
func (r *Repo) CommitTree(ctx context.Context, branch, expect, tree string, o CommitOpts) (string, error) {
	head, err := r.Head(ctx, branch)
	if err != nil {
		return "", err
	}
	if expect != "" && head != expect {
		return "", fmt.Errorf("%w: %s is at %s, expected %s", ErrStale, branch, short(head), short(expect))
	}
	msg, err := o.full()
	if err != nil {
		return "", err
	}
	when := fmt.Sprintf("%d +0000", o.when().Unix())
	out, err := r.run(ctx, strings.NewReader(msg), nil, []string{
		"GIT_AUTHOR_NAME=" + o.Author.Name, "GIT_AUTHOR_EMAIL=" + o.Author.Email, "GIT_AUTHOR_DATE=" + when,
		"GIT_COMMITTER_NAME=" + o.Committer.Name, "GIT_COMMITTER_EMAIL=" + o.Committer.Email, "GIT_COMMITTER_DATE=" + when,
	}, "commit-tree", tree, "-p", head, "-F", "-")
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(string(out))
	if err := r.setRef(ctx, branch, sha, head); err != nil {
		return "", fmt.Errorf("%w: %s moved while committing", ErrStale, branch)
	}
	return sha, nil
}

// RootCommit creates a commit without parent holding exactly files and
// returns its SHA without pointing any branch at it (spec §9.1: the owner
// baseline, later merged into every database branch).
func (r *Repo) RootCommit(ctx context.Context, files map[string][]byte, o CommitOpts) (string, error) {
	changes := make([]Change, 0, len(files))
	for p, c := range files {
		changes = append(changes, Change{Path: p, Content: c})
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return r.makeCommit(ctx, "", "", changes, o)
}

// HasPath reports whether ref contains the file or directory p.
func (r *Repo) HasPath(ctx context.Context, ref, p string) bool {
	_, err := r.git(ctx, nil, nil, "rev-parse", "--verify", "--quiet", ref+":"+p)
	return err == nil
}

// MergeDir records on branch a merge of commit from that takes only from's
// directory dir: the new tree is the branch tree plus dir. The branch must not
// have dir yet, so no file of the branch changes. When expect is not empty it
// must equal the current head, otherwise ErrStale.
func (r *Repo) MergeDir(ctx context.Context, branch, expect, from, dir string, o CommitOpts) (string, error) {
	head, err := r.Head(ctx, branch)
	if err != nil {
		return "", err
	}
	if expect != "" && head != expect {
		return "", fmt.Errorf("%w: %s is at %s, expected %s", ErrStale, branch, short(head), short(expect))
	}
	if err := ValidatePath(dir); err != nil {
		return "", err
	}
	if r.HasPath(ctx, head, dir) {
		return "", fmt.Errorf("%w: %s already has %s/", ErrExists, branch, dir)
	}
	out, err := r.git(ctx, nil, nil, "rev-parse", "--verify", "--quiet", from+":"+dir)
	if err != nil {
		return "", fmt.Errorf("gitops: %s has no directory %s", short(from), dir)
	}
	tree := strings.TrimSpace(string(out))
	sha, err := r.makeCommit(ctx, head, from, []Change{{Path: dir, Tree: tree}}, o)
	if err != nil {
		return "", err
	}
	if err := r.setRef(ctx, branch, sha, head); err != nil {
		return "", fmt.Errorf("%w: %s moved while committing", ErrStale, branch)
	}
	return sha, nil
}

// Rewind moves branch back from `from` to `to`, locally and, when remote is
// true, on the remote with a lease on `from`: it undoes an init whose push
// went through but whose registration failed, and never overwrites a commit
// someone else pushed meanwhile.
func (r *Repo) Rewind(ctx context.Context, branch, from, to string, remote bool) error {
	var firstErr error
	if remote {
		if _, err := r.git(ctx, nil, r.credArgs(), "push", "--quiet", "--force-with-lease=refs/heads/"+branch+":"+from,
			"origin", to+":refs/heads/"+branch); err != nil {
			firstErr = err
		}
	}
	if err := r.setRef(ctx, branch, to, from); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// Files lists every file path of ref.
func (r *Repo) Files(ctx context.Context, ref string) ([]string, error) {
	out, err := r.git(ctx, nil, nil, "ls-tree", "-r", "--name-only", "-z", ref)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, f := range strings.Split(string(out), "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files, nil
}

// ReadFile returns the content of path at ref.
func (r *Repo) ReadFile(ctx context.Context, ref, p string) ([]byte, error) {
	if err := ValidatePath(p); err != nil {
		return nil, err
	}
	return r.git(ctx, nil, nil, "cat-file", "blob", ref+":"+p)
}

// Snapshot streams ref's tree as tar.gz (spec §8, §9.2).
func (r *Repo) Snapshot(ctx context.Context, ref string, w io.Writer) error {
	out, err := r.git(ctx, nil, nil, "archive", "--format=tar", ref)
	if err != nil {
		return err
	}
	// Re-pack through Go's gzip so we do not depend on a gzip binary, and drop
	// git's pax global header (it carries the commit id, which we send as a header).
	tr := tar.NewReader(bytes.NewReader(out))
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// TreeEntry is one file of a tree listing.
type TreeEntry struct {
	Path string
	Size int64
}

// Tree lists the files under dir ("" = everything) at ref with their sizes,
// so callers can spot stubs (a few dozen bytes) without reading every blob.
func (r *Repo) Tree(ctx context.Context, ref, dir string) ([]TreeEntry, error) {
	args := []string{"ls-tree", "-r", "-l", "-z", ref}
	if dir != "" {
		if err := ValidatePath(dir); err != nil {
			return nil, err
		}
		args = append(args, "--", dir+"/")
	}
	out, err := r.git(ctx, nil, nil, args...)
	if err != nil {
		return nil, err
	}
	var entries []TreeEntry
	for _, rec := range strings.Split(string(out), "\x00") {
		// "<mode> blob <sha> <size>\t<path>"
		meta, p, ok := strings.Cut(rec, "\t")
		if !ok {
			continue
		}
		f := strings.Fields(meta)
		if len(f) != 4 || f[1] != "blob" {
			continue
		}
		size, _ := strconv.ParseInt(f[3], 10, 64)
		entries = append(entries, TreeEntry{Path: p, Size: size})
	}
	return entries, nil
}

// ReadFiles returns the content of every path at ref with one git process.
func (r *Repo) ReadFiles(ctx context.Context, ref string, paths []string) (map[string][]byte, error) {
	var in bytes.Buffer
	for _, p := range paths {
		if err := ValidatePath(p); err != nil {
			return nil, err
		}
		if strings.ContainsAny(p, "\n\r") {
			return nil, fmt.Errorf("gitops: invalid path %q", p)
		}
		fmt.Fprintf(&in, "%s:%s\n", ref, p)
	}
	out, err := r.git(ctx, &in, nil, "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	files := make(map[string][]byte, len(paths))
	rest := out
	for _, p := range paths {
		hdr, after, ok := bytes.Cut(rest, []byte("\n"))
		if !ok {
			return nil, fmt.Errorf("gitops: short cat-file output")
		}
		f := strings.Fields(string(hdr))
		if len(f) == 2 && f[1] == "missing" {
			return nil, fmt.Errorf("%w: %s:%s", ErrNotFound, short(ref), p)
		}
		if len(f) != 3 {
			return nil, fmt.Errorf("gitops: unexpected cat-file header %q", hdr)
		}
		n, err := strconv.Atoi(f[2])
		if err != nil || len(after) < n+1 {
			return nil, fmt.Errorf("gitops: bad cat-file output for %s", p)
		}
		files[p] = append([]byte(nil), after[:n]...)
		rest = after[n+1:]
	}
	return files, nil
}
