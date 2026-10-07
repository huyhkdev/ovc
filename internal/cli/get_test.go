package cli_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ovc/internal/cli"
	"ovc/internal/config"
	"ovc/internal/gitops"
	"ovc/internal/jobs"
	"ovc/internal/layout"
	"ovc/internal/oracle/export"
	"ovc/internal/server"
	"ovc/internal/store"
)

var t0 = time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)

// fakeDB is a catalog whose objects are per owner and whose DDL is made up.
type fakeDB struct {
	mu       sync.Mutex
	objs     map[string][]export.Object // owner -> objects
	hydrated []string                   // every object Hydrate was asked for
	ddl      map[string]string          // "OWNER.NAME" -> DDL on the database now (default: made up)
}

func (f *fakeDB) setDDL(name, ddl string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ddl == nil {
		f.ddl = map[string]string{}
	}
	f.ddl[name] = ddl
}

func (f *fakeDB) HasAlias(a string) bool { return a == "dev" || a == "prd" }
func (f *fakeDB) Objects(_ context.Context, _, schema string, _ []string) ([]export.Object, error) {
	return f.objs[schema], nil
}
func (f *fakeDB) Hydrate(_ context.Context, alias, schema string, objs []export.Object, _ int) ([]export.File, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]export.File, len(objs))
	for i, o := range objs {
		out[i] = export.File{Object: o, Content: fmt.Sprintf("CREATE OR REPLACE %s %s.%s -- from %s\nIS BEGIN NULL; END;\n/\n", o.Type, schema, o.Name, alias)}
		if c, ok := f.ddl[schema+"."+o.Name]; ok {
			out[i].Content = c
		}
		f.hydrated = append(f.hydrated, schema+"."+o.Name)
	}
	return out, nil
}

func (f *fakeDB) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.hydrated)
}

type getEnv struct {
	remote string
	db     *fakeDB
	run    func(args ...string) (string, error)
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func exitCode(err error) int {
	var ce *cli.CodedError
	if errors.As(err, &ce) {
		return ce.Code
	}
	if err != nil {
		return 1
	}
	return 0
}

func newGetEnv(t *testing.T) *getEnv {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	// a clean git for both the server mirror and the dev's clone
	// a clean git; the dev's identity is git's: ovc reads it from there
	t.Setenv("GIT_CONFIG_GLOBAL", gitIdentityFile(t, "Nguyen Van A", "a@company.local"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "Dev")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "dev@company.local")
	}
	ctx := context.Background()
	remote := filepath.Join(t.TempDir(), "remote.git")
	if out, err := exec.Command("git", "init", "--bare", "--quiet", remote).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	mirror, err := gitops.Open(ctx, filepath.Join(t.TempDir(), "mirror.git"), gitops.Config{Remote: remote})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := store.OpenFile(filepath.Join(t.TempDir(), "state.json"))
	db := &fakeDB{objs: map[string][]export.Object{
		"HR": {
			{Type: layout.PackageSpec, Name: "PKG_EMPLOYEE", LastDDLTime: t0},
			{Type: layout.PackageBody, Name: "PKG_EMPLOYEE", LastDDLTime: t0},
			{Type: layout.Procedure, Name: "P_SYNC_EMPLOYEE", LastDDLTime: t0},
			{Type: layout.Procedure, Name: "P_OTHER", LastDDLTime: t0},
			{Type: layout.View, Name: "V_A", LastDDLTime: t0},
			{Type: layout.View, Name: "V_B", LastDDLTime: t0},
			{Type: layout.Table, Name: "EMPLOYEES", LastDDLTime: t0},
		},
		"QLSC": {
			{Type: layout.PackageSpec, Name: "PKG_EMPLOYEE", LastDDLTime: t0}, // same name as in HR
			{Type: layout.Function, Name: "F_QLSC", LastDDLTime: t0},
		},
	}}
	cfg := &config.Server{Envs: []string{"dev", "prd"}}
	cfg.Git.Committer.Name, cfg.Git.Committer.Email = "OVC Bot", "ovc-bot@company.local"
	jm := jobs.New()
	srv := httptest.NewServer(server.New(server.Deps{Cfg: cfg, Git: mirror, Store: st, Catalog: db, Jobs: jm}))
	t.Cleanup(func() { jm.Wait(); srv.Close() })

	t.Setenv("OVC_SERVER", srv.URL)
	run := func(args ...string) (string, error) {
		root := cli.NewRoot()
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(args)
		err := root.ExecuteContext(ctx)
		return out.String(), err
	}
	for _, s := range []string{"HR", "QLSC"} {
		if out, err := run("init", s, "--db", "dev"); err != nil {
			t.Fatalf("init %s: %v\n%s", s, err, out)
		}
	}
	return &getEnv{remote: remote, db: db, run: run}
}

// gitIdentityFile writes a git config file holding only user.name/email.
func gitIdentityFile(t *testing.T, name, email string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(p, []byte(fmt.Sprintf("[user]\n\tname = %s\n\temail = %s\n", name, email)), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// clone makes a dev working copy of branch dev and moves into it.
func (e *getEnv) clone(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "work")
	if out, err := exec.Command("git", "clone", "--quiet", "--branch", "dev", e.remote, dir).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	t.Chdir(dir)
	return dir
}

func read(t *testing.T, dir, p string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestGetOnCleanDevFastForwards(t *testing.T) {
	e := newGetEnv(t)
	work := e.clone(t)
	if !strings.HasPrefix(read(t, work, "HR/packages/PKG_EMPLOYEE.pkb"), "-- OVC:STUB") {
		t.Fatal("a fresh clone holds stubs")
	}
	out, err := e.run("get", "HR.PKG_EMPLOYEE")
	t.Logf("ovc get output:\n%s", out)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	for _, p := range []string{"HR/packages/PKG_EMPLOYEE.pks", "HR/packages/PKG_EMPLOYEE.pkb"} {
		if c := read(t, work, p); !strings.Contains(c, "CREATE OR REPLACE") || !strings.Contains(c, "HR.PKG_EMPLOYEE") {
			t.Errorf("%s = %q", p, c)
		}
	}
	if strings.Contains(read(t, work, "QLSC/packages/PKG_EMPLOYEE.pks"), "CREATE") {
		t.Error("QLSC's package of the same name must not be touched")
	}
	if !strings.Contains(out, "fast-forwarded") || !strings.Contains(out, "2 of 2 file(s) have their content locally") {
		t.Errorf("output:\n%s", out)
	}
	// the local branch is exactly the remote one: the hydrate commit itself
	if gitIn(t, work, "rev-parse", "HEAD") != gitIn(t, e.remote, "rev-parse", "dev") {
		t.Error("local dev must equal remote dev after a fast-forward")
	}
	if s := gitIn(t, work, "status", "--porcelain"); s != "" {
		t.Errorf("working copy not clean:\n%s", s)
	}
}

func TestGetOnFeatureBranchKeepsTheMRClean(t *testing.T) {
	e := newGetEnv(t)
	work := e.clone(t)
	if _, err := e.run("get", "HR/procedures/P_SYNC_EMPLOYEE.prc"); err != nil {
		t.Fatal(err)
	}
	gitIn(t, work, "checkout", "-q", "-b", "feature/hr/a/fix-sync")
	p := filepath.Join(work, "HR/procedures/P_SYNC_EMPLOYEE.prc")
	os.WriteFile(p, []byte(read(t, work, "HR/procedures/P_SYNC_EMPLOYEE.prc")+"-- my fix\n"), 0o644)
	gitIn(t, work, "commit", "-qam", "fix sync")

	// on the feature branch, by bare object name, from inside a sub folder
	t.Chdir(filepath.Join(work, "HR", "procedures"))
	out, err := e.run("get", "P_OTHER")
	t.Logf("ovc get output:\n%s", out)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !strings.Contains(out, "merged origin/dev into feature/hr/a/fix-sync") {
		t.Errorf("output:\n%s", out)
	}
	if c := read(t, work, "HR/procedures/P_OTHER.prc"); !strings.Contains(c, "CREATE OR REPLACE") {
		t.Errorf("P_OTHER = %q", c)
	}
	if !strings.HasSuffix(read(t, work, "HR/procedures/P_SYNC_EMPLOYEE.prc"), "-- my fix\n") {
		t.Error("the dev's commit was lost")
	}
	// what the merge request into dev would show: only the dev's edit
	gitIn(t, work, "push", "-q", "origin", "feature/hr/a/fix-sync")
	if d := gitIn(t, e.remote, "diff", "--name-only", "dev...feature/hr/a/fix-sync"); d != "HR/procedures/P_SYNC_EMPLOYEE.prc" {
		t.Errorf("MR diff = %q, want only the edited file", d)
	}
	if out := gitIn(t, e.remote, "merge-tree", "--write-tree", "--name-only", "dev", "feature/hr/a/fix-sync"); strings.Contains(out, "CONFLICT") {
		t.Errorf("MR would conflict:\n%s", out)
	}
}

func TestGetWhenTheRemoteAlreadyHasContent(t *testing.T) {
	e := newGetEnv(t)
	e.clone(t)
	if _, err := e.run("get", "HR.PKG_EMPLOYEE"); err != nil {
		t.Fatal(err)
	}
	head := gitIn(t, e.remote, "rev-parse", "dev")

	// another dev, with an older clone: nothing to hydrate, just bring it in
	other := filepath.Join(t.TempDir(), "other")
	gitIn(t, filepath.Dir(other), "clone", "-q", "--branch", "dev", e.remote, other)
	gitIn(t, other, "reset", "-q", "--hard", "HEAD~1") // behind the hydrate commit
	t.Chdir(other)
	out, err := e.run("get", "HR/packages/PKG_EMPLOYEE.pkb")
	if err != nil {
		t.Fatalf("get: %v\n%s", err, out)
	}
	if gitIn(t, e.remote, "rev-parse", "dev") != head {
		t.Error("nothing changed on the database: no new commit expected")
	}
	if !strings.Contains(out, "up to date with the database: 1") || !strings.Contains(out, "fast-forwarded") {
		t.Errorf("output:\n%s", out)
	}
	if !strings.Contains(read(t, other, "HR/packages/PKG_EMPLOYEE.pkb"), "CREATE OR REPLACE") {
		t.Error("content not brought in")
	}
}

func TestGetFoldersAndTypes(t *testing.T) {
	e := newGetEnv(t)
	work := e.clone(t)
	out, err := e.run("get", "HR", "--type", "view")
	if err != nil {
		t.Fatalf("get: %v\n%s", err, out)
	}
	if !strings.Contains(read(t, work, "HR/views/V_A.sql"), "CREATE") || !strings.Contains(read(t, work, "HR/views/V_B.sql"), "CREATE") {
		t.Error("views not fetched")
	}
	if strings.Contains(read(t, work, "HR/procedures/P_OTHER.prc"), "CREATE") {
		t.Error("--type view fetched a procedure")
	}
	if _, err := e.run("get", "HR/procedures/"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"HR/procedures/P_OTHER.prc", "HR/procedures/P_SYNC_EMPLOYEE.prc"} {
		if !strings.Contains(read(t, work, p), "CREATE") {
			t.Errorf("%s not fetched", p)
		}
	}
	if _, err := e.run("get", "QLSC/procedures/"); exitCode(err) != cli.ExitUsage {
		t.Errorf("folder that does not exist: %v", err)
	}
	if _, err := e.run("get", "HR", "--type", "function"); exitCode(err) != cli.ExitUsage {
		t.Errorf("no object of that type: %v", err)
	}
}

func TestGetErrors(t *testing.T) {
	e := newGetEnv(t)
	work := e.clone(t)
	_, err := e.run("get", "PKG_EMPLOYEE")
	if exitCode(err) != cli.ExitUsage || !strings.Contains(err.Error(), "HR.PKG_EMPLOYEE, QLSC.PKG_EMPLOYEE") {
		t.Errorf("ambiguous name: %v", err)
	}
	if _, err := e.run("get", "NO_SUCH_THING"); exitCode(err) != cli.ExitUsage {
		t.Errorf("unknown name: %v", err)
	}
	// a stub edited by hand and not committed: git refuses to overwrite it
	os.WriteFile(filepath.Join(work, "HR/views/V_A.sql"), []byte("hand made\n"), 0o644)
	out, err := e.run("get", "HR/views/V_A.sql")
	if exitCode(err) != cli.ExitConflict || !strings.Contains(err.Error(), "the content is on the remote") {
		t.Errorf("local edit on the stub: code=%d %v\n%s", exitCode(err), err, out)
	}
	if read(t, work, "HR/views/V_A.sql") != "hand made\n" {
		t.Error("a local edit was overwritten")
	}
	if !strings.Contains(gitIn(t, e.remote, "show", "dev:HR/views/V_A.sql"), "CREATE") {
		t.Error("the hydrate must still be on the remote")
	}
	// outside a git working copy
	t.Chdir(t.TempDir())
	if _, err := e.run("get", "HR.PKG_EMPLOYEE"); exitCode(err) != cli.ExitUsage {
		t.Errorf("outside a repo: %v", err)
	}
}

// pushToDev commits content on the remote dev as if an MR had been merged.
func (e *getEnv) pushToDev(t *testing.T, path, content string) {
	t.Helper()
	other := filepath.Join(t.TempDir(), "merger")
	gitIn(t, filepath.Dir(other), "clone", "-q", "--branch", "dev", e.remote, other)
	os.WriteFile(filepath.Join(other, filepath.FromSlash(path)), []byte(content), 0o644)
	gitIn(t, other, "commit", "-qam", "merged MR")
	gitIn(t, other, "push", "-q", "origin", "dev")
}

func TestGetSyncsAHandEditOnTheDatabase(t *testing.T) {
	e := newGetEnv(t)
	work := e.clone(t)
	if _, err := e.run("get", "HR.P_OTHER"); err != nil {
		t.Fatal(err)
	}
	hotfix := "CREATE OR REPLACE PROCEDURE P_OTHER IS BEGIN hotfix; END;\n/\n"
	e.db.setDDL("HR.P_OTHER", hotfix)
	out, err := e.run("get", "HR.P_OTHER")
	t.Logf("ovc get output:\n%s", out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "synced (edited on the database outside OVC, now on dev): 1") || read(t, work, "HR/procedures/P_OTHER.prc") != hotfix {
		t.Errorf("hand edit not synced:\n%s", out)
	}
}

func TestGetConflictKeepsTheBranchVersionAndExits5(t *testing.T) {
	e := newGetEnv(t)
	work := e.clone(t)
	if _, err := e.run("get", "HR.P_OTHER"); err != nil {
		t.Fatal(err)
	}
	newCode := "CREATE OR REPLACE PROCEDURE P_OTHER IS BEGIN new_code; END;\n/\n"
	e.pushToDev(t, "HR/procedures/P_OTHER.prc", newCode)                                        // merged, not deployed
	e.db.setDDL("HR.P_OTHER", "CREATE OR REPLACE PROCEDURE P_OTHER IS BEGIN hotfix; END;\n/\n") // and a hand edit
	out, err := e.run("get", "HR.P_OTHER", "HR.P_SYNC_EMPLOYEE")
	t.Logf("ovc get output:\n%s", out)
	if exitCode(err) != cli.ExitConflict || !strings.Contains(err.Error(), "HR/procedures/P_OTHER.prc") {
		t.Fatalf("code=%d err=%v", exitCode(err), err)
	}
	if !strings.Contains(out, "CONFLICT HR/procedures/P_OTHER.prc") || !strings.Contains(out, "sync branch (opened): sync/dev/HR/procedures/P_OTHER.prc-") ||
		!strings.Contains(out, "to be resolved by: Dev <dev@company.local>") {
		t.Errorf("output:\n%s", out)
	}
	// the rest still arrives; the conflicting object is the branch version
	if read(t, work, "HR/procedures/P_OTHER.prc") != newCode {
		t.Errorf("P_OTHER = %q, want the branch version", read(t, work, "HR/procedures/P_OTHER.prc"))
	}
	if !strings.Contains(read(t, work, "HR/procedures/P_SYNC_EMPLOYEE.prc"), "CREATE OR REPLACE") {
		t.Error("the other object must still be fetched")
	}
	if refs := gitIn(t, e.remote, "for-each-ref", "--format=%(refname:short)", "refs/heads/sync"); !strings.HasPrefix(refs, "sync/dev/HR/procedures/P_OTHER.prc-") {
		t.Errorf("sync branch = %q", refs)
	}
}
