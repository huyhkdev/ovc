// Package gitlocal runs git in the dev's own working copy for `ovc get`
// (spec §9.8): fetch the database branch, read files on the remote side, and
// bring the branch into the current one. It uses the dev's git, config and
// credentials as they are.
package gitlocal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// ErrNotRepo means the directory is not inside a git working copy.
var ErrNotRepo = errors.New("not inside a git working copy (git clone the repository first)")

type Repo struct {
	Root string
	// Out receives the output of fetch and merge so the dev sees what git
	// did (and can answer credential prompts); nil discards it.
	Out io.Writer
}

// Open finds the working copy that contains dir.
func Open(ctx context.Context, dir string) (*Repo, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		if _, lerr := exec.LookPath("git"); lerr != nil {
			return nil, fmt.Errorf("git is not installed: %w", lerr)
		}
		return nil, ErrNotRepo
	}
	return &Repo{Root: strings.TrimSpace(string(out))}, nil
}

type gitError struct {
	args   []string
	stderr string
	err    error
}

func (e *gitError) Error() string {
	return fmt.Sprintf("git %s: %v: %s", strings.Join(e.args, " "), e.err, strings.TrimSpace(e.stderr))
}
func (e *gitError) Unwrap() error { return e.err }

// output runs git quietly and returns stdout.
func (r *Repo) output(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", r.Root}, args...)...)
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), &gitError{args, stderr.String(), err}
	}
	return stdout.Bytes(), nil
}

// visible runs git with its output shown to the dev.
func (r *Repo) visible(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", r.Root}, args...)...)
	out := r.Out
	if out == nil {
		out = io.Discard
	}
	// git writes stdout and stderr from two goroutines: serialise them.
	shared := &lockedWriter{w: out}
	var stderr bytes.Buffer
	cmd.Stdin = os.Stdin
	cmd.Stdout = shared
	cmd.Stderr = io.MultiWriter(shared, &stderr)
	if err := cmd.Run(); err != nil {
		return &gitError{args, stderr.String(), err}
	}
	return nil
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// RemoteRef is where Fetch puts the remote branch.
func RemoteRef(remote, branch string) string { return "refs/remotes/" + remote + "/" + branch }

// Fetch updates RemoteRef(remote, branch) from the remote.
func (r *Repo) Fetch(ctx context.Context, remote, branch string) error {
	return r.visible(ctx, "fetch", "--quiet", remote, "+refs/heads/"+branch+":"+RemoteRef(remote, branch))
}

// Entry is one file of a tree.
type Entry struct {
	Path string
	Size int64
}

// Tree lists every file of ref.
func (r *Repo) Tree(ctx context.Context, ref string) ([]Entry, error) {
	out, err := r.output(ctx, nil, "ls-tree", "-r", "-l", "-z", ref)
	if err != nil {
		return nil, err
	}
	var entries []Entry
	for _, rec := range strings.Split(string(out), "\x00") {
		meta, p, ok := strings.Cut(rec, "\t")
		if !ok {
			continue
		}
		f := strings.Fields(meta)
		if len(f) != 4 || f[1] != "blob" {
			continue
		}
		size, _ := strconv.ParseInt(f[3], 10, 64)
		entries = append(entries, Entry{Path: p, Size: size})
	}
	return entries, nil
}

// CurrentBranch returns the checked-out branch, or "" on a detached HEAD.
func (r *Repo) CurrentBranch(ctx context.Context) string {
	out, err := r.output(ctx, nil, "symbolic-ref", "-q", "--short", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// IsAncestor reports whether a is an ancestor of (or equal to) b.
func (r *Repo) IsAncestor(ctx context.Context, a, b string) bool {
	_, err := r.output(ctx, nil, "merge-base", "--is-ancestor", a, b)
	return err == nil
}

// MergeOutcome tells what Integrate did.
type MergeOutcome string

const (
	UpToDate    MergeOutcome = "up-to-date"   // the current branch already contains ref
	FastForward MergeOutcome = "fast-forward" // the current branch had no commits of its own
	Merged      MergeOutcome = "merged"       // a merge commit was made
)

// Integrate brings ref into the current branch like `git pull` would:
// nothing when it is already contained, a fast-forward when possible, a merge
// commit otherwise. Git refuses on its own when uncommitted changes would be
// overwritten; conflicts are left for the dev to resolve with git.
func (r *Repo) Integrate(ctx context.Context, ref string) (MergeOutcome, error) {
	if r.IsAncestor(ctx, ref, "HEAD") {
		return UpToDate, nil
	}
	if r.IsAncestor(ctx, "HEAD", ref) {
		return FastForward, r.visible(ctx, "merge", "--ff-only", "--quiet", ref)
	}
	return Merged, r.visible(ctx, "merge", "--no-edit", "--quiet", ref)
}
