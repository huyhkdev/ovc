package gitops

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	alice = Identity{Name: "Nguyen Van A", Email: "a@company.local"}
	bot   = Identity{Name: "OVC Bot", Email: "ovc-bot@company.local"}
	ctx   = context.Background()
	fixed = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	opts  = CommitOpts{Author: alice, Committer: bot, Message: "msg", When: fixed}
)

func needGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
}

// git runs git against dir (a bare repo) and fails the test on error.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), "GIT_DIR="+dir, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newMirror returns a mirror whose origin is a fresh bare repo, and that
// remote's path.
func newMirror(t *testing.T) (*Repo, string) {
	t.Helper()
	needGit(t)
	remote := filepath.Join(t.TempDir(), "remote.git")
	cmd := exec.Command("git", "init", "--bare", "--quiet", remote)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("init remote: %v\n%s", err, out)
	}
	r, err := Open(ctx, filepath.Join(t.TempDir(), "mirror.git"), Config{Remote: remote})
	if err != nil {
		t.Fatal(err)
	}
	return r, remote
}

func files(kv ...string) map[string][]byte {
	m := map[string][]byte{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i]] = []byte(kv[i+1])
	}
	return m
}

func TestCreateBranchAndRead(t *testing.T) {
	r, _ := newMirror(t)
	in := files(
		"ovc.yaml", "schema: QLSC\n",
		"packages/PKG_A.pkb", "-- OVC:STUB type=PACKAGE_BODY\n",
		"packages/K~iem~T~ra_~MST.pkb", "x\n", // encoded name
		"views/with space.sql", "v\n",
		`views/qu"ote.sql`, "q\n",
		"views/ünï.sql", "u\n",
	)
	sha, err := r.CreateBranch(ctx, "qlsc_dev", in, opts)
	if err != nil {
		t.Fatal(err)
	}
	if head, _ := r.Head(ctx, "qlsc_dev"); head != sha {
		t.Errorf("head %s != %s", head, sha)
	}
	got, err := r.Files(ctx, "qlsc_dev")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{}
	for p := range in {
		want = append(want, p)
	}
	sort.Strings(want)
	sort.Strings(got)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("files\n got %q\nwant %q", got, want)
	}
	for p, c := range in {
		b, err := r.ReadFile(ctx, "qlsc_dev", p)
		if err != nil || !bytes.Equal(b, c) {
			t.Errorf("ReadFile(%q) = %q, %v", p, b, err)
		}
	}
	// orphan: no parent; identities and date as given
	if n := gitOut(t, r.dir, "rev-list", "--count", "qlsc_dev"); n != "1" {
		t.Errorf("want a single root commit, got %s", n)
	}
	if p := gitOut(t, r.dir, "log", "-1", "--format=%an <%ae>|%cn <%ce>|%at", "qlsc_dev"); p != "Nguyen Van A <a@company.local>|OVC Bot <ovc-bot@company.local>|"+fmt.Sprint(fixed.Unix()) {
		t.Errorf("identity/date = %q", p)
	}
	if _, err := r.CreateBranch(ctx, "qlsc_dev", in, opts); !errors.Is(err, ErrExists) {
		t.Errorf("second create: %v, want ErrExists", err)
	}
	if _, err := r.Head(ctx, "nope"); !errors.Is(err, ErrNoBranch) {
		t.Errorf("Head(nope) = %v", err)
	}
}

func TestBranchFromSharesBaseline(t *testing.T) {
	r, _ := newMirror(t)
	dev, _ := r.CreateBranch(ctx, "qlsc_dev", files("a.sql", "1\n"), opts)
	prd, err := r.BranchFrom(ctx, "qlsc_prd", "qlsc_dev")
	if err != nil || prd != dev {
		t.Fatalf("BranchFrom = %s, %v", prd, err)
	}
	if _, err := r.BranchFrom(ctx, "qlsc_prd", "qlsc_dev"); !errors.Is(err, ErrExists) {
		t.Errorf("want ErrExists, got %v", err)
	}
	if _, err := r.BranchFrom(ctx, "x_dev", "missing"); err == nil {
		t.Error("unknown ref must fail")
	}
}

func TestCommitChangesTrailersAndCAS(t *testing.T) {
	r, _ := newMirror(t)
	base, _ := r.CreateBranch(ctx, "qlsc_dev", files("a.sql", "1\n", "b.sql", "2\n", "d/c.sql", "3\n"), opts)
	o := opts
	o.Message = "update objects"
	o.Trailers = []Trailer{{"OVC-User", "a@company.local"}, {"OVC-Hydrate", "objects=a.sql"}}
	sha, err := r.Commit(ctx, "qlsc_dev", base, []Change{
		{Path: "a.sql", Content: []byte("1-new\n")},
		{Path: "b.sql", Delete: true},
		{Path: "e/new.sql", Content: []byte("n\n")},
	}, o)
	if err != nil {
		t.Fatal(err)
	}
	if a, _ := r.ReadFile(ctx, "qlsc_dev", "a.sql"); string(a) != "1-new\n" {
		t.Errorf("a.sql = %q", a)
	}
	if _, err := r.ReadFile(ctx, "qlsc_dev", "b.sql"); err == nil {
		t.Error("b.sql should be deleted")
	}
	if c, _ := r.ReadFile(ctx, "qlsc_dev", "d/c.sql"); string(c) != "3\n" {
		t.Errorf("untouched file changed: %q", c)
	}
	msg := gitOut(t, r.dir, "log", "-1", "--format=%B", "qlsc_dev")
	if !strings.Contains(msg, "update objects\n\nOVC-User: a@company.local\nOVC-Hydrate: objects=a.sql") {
		t.Errorf("trailers missing:\n%s", msg)
	}
	if tr := gitOut(t, r.dir, "log", "-1", "--format=%(trailers:key=OVC-User,valueonly)", "qlsc_dev"); tr != "a@company.local" {
		t.Errorf("git does not parse our trailer: %q", tr)
	}
	if parent := gitOut(t, r.dir, "rev-parse", "qlsc_dev^"); parent != base {
		t.Errorf("parent %s != base %s", parent, base)
	}
	// stale expectation
	if _, err := r.Commit(ctx, "qlsc_dev", base, []Change{{Path: "z.sql", Content: []byte("z")}}, opts); !errors.Is(err, ErrStale) {
		t.Errorf("want ErrStale, got %v", err)
	}
	if h, _ := r.Head(ctx, "qlsc_dev"); h != sha {
		t.Error("branch moved by a failed commit")
	}
	// empty message / bad identity / bad paths
	bad := opts
	bad.Message = " "
	if _, err := r.Commit(ctx, "qlsc_dev", "", nil, bad); err == nil {
		t.Error("empty message must fail")
	}
	bad = opts
	bad.Author = Identity{Name: "x", Email: "a>b"}
	if _, err := r.Commit(ctx, "qlsc_dev", "", nil, bad); err == nil {
		t.Error("bad identity must fail")
	}
	for _, p := range []string{"../x", "/abs", "a/../b", ".git/config", "a//b", `a\b`, ""} {
		if _, err := r.Commit(ctx, "qlsc_dev", "", []Change{{Path: p, Content: []byte("x")}}, opts); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("path %q: want ErrInvalidPath, got %v", p, err)
		}
	}
}

func TestPushFetchAcrossMirrors(t *testing.T) {
	r1, remote := newMirror(t)
	sha, _ := r1.CreateBranch(ctx, "qlsc_dev", files("a.sql", "1\n"), opts)
	if err := r1.Push(ctx, "qlsc_dev"); err != nil {
		t.Fatal(err)
	}
	r2, err := Open(ctx, filepath.Join(t.TempDir(), "m2.git"), Config{Remote: remote})
	if err != nil {
		t.Fatal(err)
	}
	if err := r2.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	if h, err := r2.Head(ctx, "qlsc_dev"); err != nil || h != sha {
		t.Fatalf("second mirror head = %s, %v", h, err)
	}
	// r2 advances the remote; r1 is now behind and its push must be rejected.
	sha2, _ := r2.Commit(ctx, "qlsc_dev", "", []Change{{Path: "a.sql", Content: []byte("2\n")}}, opts)
	if err := r2.Push(ctx, "qlsc_dev"); err != nil {
		t.Fatal(err)
	}
	if _, err := r1.Commit(ctx, "qlsc_dev", "", []Change{{Path: "b.sql", Content: []byte("b\n")}}, opts); err != nil {
		t.Fatal(err)
	}
	if err := r1.Push(ctx, "qlsc_dev"); !errors.Is(err, ErrRejected) {
		t.Fatalf("want ErrRejected, got %v", err)
	}
	// after a forced refresh the mirror matches the remote again
	if err := r1.Fetch(ctx, "qlsc_dev"); err != nil {
		t.Fatal(err)
	}
	if h, _ := r1.Head(ctx, "qlsc_dev"); h != sha2 {
		t.Errorf("Fetch did not reset the cache: %s != %s", h, sha2)
	}
	// switching host = changing the remote of an existing mirror
	r3, err := Open(ctx, r1.dir, Config{Remote: remote + "-other"})
	if err != nil {
		t.Fatal(err)
	}
	if got := gitOut(t, r3.dir, "remote", "get-url", "origin"); got != remote+"-other" {
		t.Errorf("remote not updated: %s", got)
	}
}

func TestSnapshotTarGz(t *testing.T) {
	r, _ := newMirror(t)
	in := files("ovc.yaml", "schema: X\n", "packages/P.pks", "spec\n", "views/V.sql", "view\n")
	r.CreateBranch(ctx, "x_dev", in, opts)
	var buf bytes.Buffer
	if err := r.Snapshot(ctx, "x_dev", &buf); err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	got := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		b, _ := io.ReadAll(tr)
		got[h.Name] = string(b)
	}
	if len(got) != len(in) {
		t.Fatalf("got %v", got)
	}
	for p, c := range in {
		if got[p] != string(c) {
			t.Errorf("%s = %q, want %q", p, got[p], c)
		}
	}
}

func TestLockSerialises(t *testing.T) {
	r, _ := newMirror(t)
	var mu sync.Mutex
	inside, maxInside := 0, 0
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock, err := r.Lock(ctx, "qlsc_dev")
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			inside++
			if inside > maxInside {
				maxInside = inside
			}
			mu.Unlock()
			time.Sleep(5 * time.Millisecond)
			mu.Lock()
			inside--
			mu.Unlock()
			unlock()
		}()
	}
	wg.Wait()
	if maxInside != 1 {
		t.Errorf("%d holders at once", maxInside)
	}
	// different branches do not block each other; a cancelled ctx stops waiting
	u1, _ := r.Lock(ctx, "a_dev")
	u2, err := r.Lock(ctx, "b_dev")
	if err != nil {
		t.Fatal(err)
	}
	u2()
	c, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, err := r.Lock(c, "a_dev"); err == nil {
		t.Error("lock should time out")
	}
	u1()
}

func TestConcurrentCommitsOnOneBranchNeverLoseWrites(t *testing.T) {
	r, _ := newMirror(t)
	r.CreateBranch(ctx, "qlsc_dev", files("a.sql", "1\n"), opts)
	var wg sync.WaitGroup
	var okN, staleN int
	var mu sync.Mutex
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// no Lock on purpose: the compare-and-swap alone must keep history linear
			_, err := r.Commit(ctx, "qlsc_dev", "", []Change{{Path: fmt.Sprintf("f%d.sql", i), Content: []byte("x")}}, opts)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				okN++
			case errors.Is(err, ErrStale):
				staleN++
			default:
				t.Errorf("unexpected: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if okN+staleN != 6 || okN == 0 {
		t.Fatalf("ok=%d stale=%d", okN, staleN)
	}
	fs, _ := r.Files(ctx, "qlsc_dev")
	if len(fs) != 1+okN {
		t.Errorf("%d files but %d successful commits: a write was lost", len(fs), okN)
	}
	if n := gitOut(t, r.dir, "rev-list", "--count", "qlsc_dev"); n != fmt.Sprint(1+okN) {
		t.Errorf("history has %s commits, want %d", n, 1+okN)
	}
}

func TestTokenNeverStoredOrPrinted(t *testing.T) {
	needGit(t)
	const token = "ghp_S3cr3tT0ken"
	dir := filepath.Join(t.TempDir(), "m.git")
	r, err := Open(ctx, dir, Config{Remote: "https://example.invalid/org/repo.git", Token: token})
	if err != nil {
		t.Fatal(err)
	}
	// nothing on disk contains the token
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), token) {
				t.Errorf("token written to %s", p)
			}
		}
		return nil
	})
	// a failing network call does not echo it, and does not hang on a prompt
	c, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	err = r.Fetch(c)
	if err == nil {
		t.Fatal("fetch of an invalid host must fail")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("token leaked in error: %v", err)
	}
	for _, a := range r.credArgs() {
		if strings.Contains(a, token) {
			t.Errorf("token in command line args: %q", a)
		}
	}
}

// The owner-baseline model of spec §9.1: one root commit per owner, merged as
// a folder into every database branch; changes on one branch then merge into
// the other cleanly, and other owners' folders never leak across.
func TestRootCommitMergeDirAndRewind(t *testing.T) {
	r, remote := newMirror(t)
	hr, err := r.RootCommit(ctx, files("HR/packages/P.pkb", "-- OVC:STUB type=PACKAGE_BODY\n", "HR/ovc.yaml", "schema: HR\n"), opts)
	if err != nil {
		t.Fatal(err)
	}
	qlsc, _ := r.RootCommit(ctx, files("QLSC/tables/T.sql", "-- OVC:STUB type=TABLE\n"), opts)

	// dev: created at QLSC's baseline, then HR merged in
	if _, err := r.BranchFrom(ctx, "dev", qlsc); err != nil {
		t.Fatal(err)
	}
	m, err := r.MergeDir(ctx, "dev", qlsc, hr, "HR", opts)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := r.Files(ctx, "dev"); strings.Join(got, ",") != "HR/ovc.yaml,HR/packages/P.pkb,QLSC/tables/T.sql" {
		t.Errorf("dev files = %v", got)
	}
	if _, err := r.MergeDir(ctx, "dev", m, hr, "HR", opts); !errors.Is(err, ErrExists) {
		t.Errorf("merging HR twice = %v", err)
	}
	if _, err := r.MergeDir(ctx, "dev", "deadbeef", hr, "X", opts); !errors.Is(err, ErrStale) {
		t.Errorf("stale expect = %v", err)
	}
	// prd: only HR
	r.BranchFrom(ctx, "prd", hr)
	for _, b := range []string{"dev", "prd"} {
		if err := r.Push(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	if p := gitOut(t, remote, "log", "-1", "--format=%P", "dev"); p != qlsc+" "+hr {
		t.Errorf("merge parents = %q", p)
	}
	// edit HR on dev, merge dev -> prd: clean; it brings QLSC too (a real
	// dev->prd PR would), but HR content merges without conflict
	dev, _ := r.Head(ctx, "dev")
	r.Commit(ctx, "dev", dev, []Change{{Path: "HR/packages/P.pkb", Content: []byte("CREATE OR REPLACE PACKAGE BODY P AS END;\n/\n")}}, opts)
	r.Push(ctx, "dev")
	if out := gitOut(t, remote, "merge-tree", "--write-tree", "--name-only", "prd", "dev"); strings.Contains(out, "CONFLICT") {
		t.Errorf("dev -> prd conflicts:\n%s", out)
	}

	// rewind: undo a pushed commit, but never over someone else's push
	before, _ := r.Head(ctx, "prd")
	after, _ := r.Commit(ctx, "prd", before, []Change{{Path: "HR/x.sql", Content: []byte("x")}}, opts)
	r.Push(ctx, "prd")
	if err := r.Rewind(ctx, "prd", after, before, true); err != nil {
		t.Fatal(err)
	}
	if h := gitOut(t, remote, "rev-parse", "prd"); h != before {
		t.Errorf("remote prd = %s, want %s", h, before)
	}
	if err := r.Rewind(ctx, "prd", after, before, true); err == nil {
		t.Error("rewind with a stale lease must fail")
	}
}

func TestTreeAndReadFiles(t *testing.T) {
	r, _ := newMirror(t)
	if _, err := r.CreateBranch(ctx, "dev", files("HR/a.sql", "-- OVC:STUB type=VIEW\n", "HR/sub dir/b c.prc", "body\nwith\x00nul",
		"QLSC/x.sql", "x", ".gitlab-ci.yml", "ci"), opts); err != nil {
		t.Fatal(err)
	}
	tree, err := r.Tree(ctx, "dev", "HR")
	if err != nil || len(tree) != 2 || tree[0].Path != "HR/a.sql" || tree[0].Size != 22 || tree[1].Path != "HR/sub dir/b c.prc" {
		t.Fatalf("tree = %+v, %v", tree, err)
	}
	all, _ := r.Tree(ctx, "dev", "")
	if len(all) != 4 {
		t.Errorf("full tree = %+v", all)
	}
	got, err := r.ReadFiles(ctx, "dev", []string{"HR/sub dir/b c.prc", "HR/a.sql"})
	if err != nil || string(got["HR/a.sql"]) != "-- OVC:STUB type=VIEW\n" || string(got["HR/sub dir/b c.prc"]) != "body\nwith\x00nul" {
		t.Errorf("ReadFiles = %q, %v", got, err)
	}
	if _, err := r.ReadFiles(ctx, "dev", []string{"HR/nope.sql"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing file = %v", err)
	}
}

func TestRemoteBranchesAndAuthors(t *testing.T) {
	r, _ := newMirror(t)
	base, _ := r.CreateBranch(ctx, "dev", files("HR/p.prc", "v1"), opts)
	bob := Identity{Name: "Bob", Email: "bob@x"}
	r.Commit(ctx, "dev", "", []Change{{Path: "HR/p.prc", Content: []byte("v2")}}, CommitOpts{Author: bob, Committer: bot, Message: "m"})
	r.Commit(ctx, "dev", "", []Change{{Path: "HR/q.prc", Content: []byte("x")}}, opts)
	r.Commit(ctx, "dev", "", []Change{{Path: "HR/p.prc", Content: []byte("v3")}}, opts)
	for _, b := range []string{"dev", "sync/dev/HR/p-1", "sync/dev/HR/p-2", "sync/prd/HR/p-1"} {
		if b != "dev" {
			r.BranchFrom(ctx, b, base)
		}
		if err := r.Push(ctx, b); err != nil {
			t.Fatal(err)
		}
	}
	got, err := r.RemoteBranches(ctx, "sync/dev/HR/p-")
	if err != nil || strings.Join(got, ",") != "sync/dev/HR/p-1,sync/dev/HR/p-2" {
		t.Errorf("remote branches = %v, %v", got, err)
	}
	r.DeleteBranch(ctx, "sync/dev/HR/p-1", true)
	if got, _ := r.RemoteBranches(ctx, "sync/dev/HR/p-"); strings.Join(got, ",") != "sync/dev/HR/p-2" {
		t.Errorf("after delete = %v", got)
	}
	if got, _ := r.RemoteBranches(ctx, "sync/dev/QLSC/"); len(got) != 0 {
		t.Errorf("none = %v", got)
	}
	a, err := r.Authors(ctx, base, "dev", "HR/p.prc")
	if err != nil || strings.Join(a, ",") != "Nguyen Van A <a@company.local>,Bob <bob@x>" {
		t.Errorf("authors = %v, %v", a, err)
	}
}
