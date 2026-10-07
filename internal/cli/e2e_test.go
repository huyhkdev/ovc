package cli_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"ovc/internal/catalog"
	"ovc/internal/ci"
	"ovc/internal/cli"
	"ovc/internal/config"
	"ovc/internal/gitops"
	"ovc/internal/jobs"
	"ovc/internal/oracle/export"
	"ovc/internal/server"
	"ovc/internal/store"
)

// End to end: real CLI -> real server -> real Oracle (Docker fixture) -> real
// git (bare repo on disk). Needs: deploy/dev-oracle.sh && source .ovc-dev.env
func TestInitEndToEnd(t *testing.T) {
	if os.Getenv("OVC_IT_HOST") == "" {
		t.Skip("OVC_IT_* not set (run deploy/dev-oracle.sh and source .ovc-dev.env)")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
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
	port, _ := strconv.Atoi(os.Getenv("OVC_IT_PORT"))
	db := config.Database{
		DSN:         os.Getenv("OVC_IT_HOST") + ":" + strconv.Itoa(port) + "/" + os.Getenv("OVC_IT_SERVICE"),
		User:        os.Getenv("OVC_IT_USER"),
		PasswordEnv: "OVC_IT_PASSWORD",
	}
	// one fixture DB plays both envs
	cfg := &config.Server{Envs: []string{"dev", "prd"}, Exclude: ci.DefaultExclude,
		Databases: map[string]config.Database{"dev": db, "prd": db}}
	cfg.Git.Committer.Name, cfg.Git.Committer.Email = "OVC Bot", "ovc-bot@company.local"
	cfg.CI.GitHubTemplate, cfg.CI.GitLabProject = "org/ovc-ci-templates/.github/workflows/oracle-deploy.yml@v1", "db/ovc-ci-templates"
	jm := jobs.New()
	srv := httptest.NewServer(server.New(server.Deps{Cfg: cfg, Git: mirror, Store: st, Jobs: jm,
		Catalog: catalog.Oracle{DBs: cfg.Databases}}))
	defer func() { jm.Wait(); srv.Close() }()

	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("OVC_SERVER", "")
	run := func(args ...string) (string, error) {
		root := cli.NewRoot()
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(args)
		err := root.ExecuteContext(ctx)
		return out.String(), err
	}
	code := func(err error) int {
		var ce *cli.CodedError
		if errors.As(err, &ce) {
			return ce.Code
		}
		if err != nil {
			return 1
		}
		return 0
	}

	// no server built in, then no git identity -> exit 3 with instructions
	if out, err := run("init", "HR", "--db", "dev"); code(err) != cli.ExitNotLoggedIn || !strings.Contains(err.Error(), "OVC_SERVER") {
		t.Fatalf("no server: code=%d err=%v out=%s", code(err), err, out)
	}
	t.Setenv("OVC_SERVER", srv.URL)
	if out, err := run("init", "HR", "--db", "dev"); code(err) != cli.ExitNotLoggedIn || !strings.Contains(err.Error(), "git config --global user.name") {
		t.Fatalf("no git identity: code=%d err=%v out=%s", code(err), err, out)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", gitIdentityFile(t, "Nguyen Van A", "a@company.local"))
	if out, _ := run("whoami"); !strings.Contains(out, "Nguyen Van A <a@company.local>") || !strings.Contains(out, srv.URL) {
		t.Errorf("whoami = %s", out)
	}

	// a procedure this test owns, created before init and edited "by hand" later
	var owner *sql.DB
	if pw := os.Getenv("OVC_IT_OWNER_PASSWORD"); pw != "" {
		owner, err = export.Open(ctx, export.Config{Host: os.Getenv("OVC_IT_HOST"), Port: port, Service: os.Getenv("OVC_IT_SERVICE"),
			User: os.Getenv("OVC_IT_SCHEMA"), Password: pw})
		if err != nil {
			t.Fatal(err)
		}
		defer owner.Close()
		if _, err := owner.ExecContext(ctx, "CREATE OR REPLACE PROCEDURE P_OVC_E2E IS BEGIN NULL; END;"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { owner.ExecContext(context.Background(), "DROP PROCEDURE P_OVC_E2E") })
	}

	out, err := run("init", "HR", "--db", "dev")
	t.Logf("ovc init output:\n%s", out)
	if err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	for _, want := range []string{"OK HR on db dev:", "branch   dev (new branch)", "folder   HR/", "first database of HR", "PACKAGE BODY"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q", want)
		}
	}

	git := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(), "GIT_DIR="+remote, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, b)
		}
		return strings.TrimSpace(string(b))
	}
	tree := map[string]bool{}
	for _, f := range strings.Split(git("ls-tree", "-r", "--name-only", "refs/heads/dev"), "\n") {
		tree[f] = true
	}
	for _, f := range []string{
		"packages/PKG_EMPLOYEE.pks", "packages/PKG_EMPLOYEE.pkb", "procedures/P_SYNC_EMPLOYEE.prc",
		"functions/F_GET_SALARY.fnc", "triggers/TRG_EMPLOYEES_BIU.trg", "views/V_EMPLOYEE_INFO.sql",
		"materialized_views/MV_EMP_COUNT.sql", "types/T_EMP_REC.tps", "synonyms/SYN_DEPT.sql",
		"tables/EMPLOYEES.sql", "tables/DEPARTMENTS.sql", "sequences/EMPLOYEES_SEQ.sql",
		"indexes/EMPLOYEES_IX1.sql", "constraints/EMPLOYEES.sql", "ovc.yaml",
	} {
		if !tree["HR/"+f] {
			t.Errorf("missing HR/%s", f)
		}
	}
	for _, f := range []string{".github/workflows/ovc-deploy.yml", ".gitlab-ci.yml"} {
		if !tree[f] {
			t.Errorf("missing %s at the branch root", f)
		}
	}
	for f := range tree {
		if strings.Contains(f, "TMP_SCRATCH") || strings.Contains(f, "SYS_") {
			t.Errorf("excluded object leaked into the repo: %s", f)
		}
		if strings.HasSuffix(f, ".sql") && strings.HasPrefix(f, "HR/tables/MV_") {
			t.Errorf("MV container table listed as a table: %s", f)
		}
	}
	if refs := git("for-each-ref", "--format=%(refname)", "refs/heads"); refs != "refs/heads/dev" {
		t.Errorf("init --db dev must create only dev, have:\n%s", refs)
	}
	if s := git("show", "refs/heads/dev:HR/tables/EMPLOYEES.sql"); s != "-- OVC:STUB type=TABLE" {
		t.Errorf("not a stub: %q", s)
	}
	if a := git("log", "-1", "--format=%an|%ae|%cn", "refs/heads/dev"); a != "Nguyen Van A|a@company.local|OVC Bot" {
		t.Errorf("authorship = %q", a)
	}

	// the second database: same DB here, so HR/ is exactly the owner baseline
	out, err = run("init", "HR", "--db", "prd")
	t.Logf("ovc init --db prd output:\n%s", out)
	if err != nil || !strings.Contains(out, "branch   prd (new branch)") || !strings.Contains(out, "+0 -0 stubs") {
		t.Fatalf("init prd: %v\n%s", err, out)
	}
	if git("rev-parse", "refs/heads/dev:HR") != git("rev-parse", "refs/heads/prd:HR") {
		t.Error("identical DBs: HR/ must be the same tree on dev and prd")
	}
	if git("merge-base", "refs/heads/dev", "refs/heads/prd") != git("rev-parse", "refs/heads/dev~1") {
		t.Error("dev and prd must share the owner baseline")
	}

	// listing, and initialising a schema on a database again is a conflict (exit 5)
	if out, err := run("schemas"); err != nil || !strings.Contains(out, "HR  (folder HR/)") || !strings.Contains(out, "branch dev") || !strings.Contains(out, "branch prd") {
		t.Errorf("schemas: %v %q", err, out)
	}
	if _, err := run("init", "hr", "--db", "dev"); code(err) != cli.ExitConflict {
		t.Errorf("second init: code=%d err=%v, want %d", code(err), err, cli.ExitConflict)
	}
	// a dev clones with git and fetches real DDL with ovc get
	work := filepath.Join(t.TempDir(), "work")
	if b, err := exec.Command("git", "clone", "--quiet", "--branch", "dev", remote, work).CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v\n%s", err, b)
	}
	t.Chdir(work)
	out, err = run("get", "HR.PKG_EMPLOYEE", "HR/tables/EMPLOYEES.sql")
	t.Logf("ovc get output:\n%s", out)
	if err != nil {
		t.Fatalf("get: %v\n%s", err, out)
	}
	for p, want := range map[string]string{
		"HR/packages/PKG_EMPLOYEE.pks":      "CREATE OR REPLACE PACKAGE",
		"HR/packages/PKG_EMPLOYEE.pkb":      "CREATE OR REPLACE PACKAGE BODY",
		"HR/tables/EMPLOYEES.sql":           "CREATE TABLE",
		"HR/indexes/EMPLOYEES_IX1.sql":      "CREATE INDEX", // a table brings its indexes
		"HR/constraints/EMPLOYEES.sql":      "FOREIGN KEY",  // and its foreign keys
		"HR/procedures/P_SYNC_EMPLOYEE.prc": "-- OVC:STUB",  // not asked for
	} {
		b, err := os.ReadFile(filepath.Join(work, filepath.FromSlash(p)))
		if err != nil || !strings.Contains(string(b), want) {
			t.Errorf("%s does not contain %q:\n%s", p, want, b)
		}
	}
	if strings.Contains(git("show", "refs/heads/dev:HR/packages/PKG_EMPLOYEE.pkb"), `"HR".`) {
		t.Error("hydrated DDL must not carry the schema name")
	}
	if tr := git("log", "-1", "--format=%an|%cn|%(trailers:key=OVC-Hydrate,valueonly)", "refs/heads/dev"); tr != "Nguyen Van A|OVC Bot|HR db=dev objects=5" {
		t.Errorf("hydrate commit = %q", tr)
	}

	// asked again: the database did not change, so nothing is written (the
	// normalised DDL must be byte-stable or every get would report a sync)
	head := git("rev-parse", "refs/heads/dev")
	out, err = run("get", "HR.PKG_EMPLOYEE", "HR/tables/EMPLOYEES.sql")
	if err != nil || !strings.Contains(out, "up to date with the database: 5") || git("rev-parse", "refs/heads/dev") != head {
		t.Errorf("second get: %v\n%s", err, out)
	}
	// a hand edit on the database is synced onto the branch and arrives locally
	if owner != nil {
		if _, err := run("get", "HR.P_OVC_E2E"); err != nil {
			t.Fatal(err)
		}
		if _, err := owner.ExecContext(ctx, "CREATE OR REPLACE PROCEDURE P_OVC_E2E IS BEGIN DBMS_OUTPUT.PUT_LINE('hotfix'); END;"); err != nil {
			t.Fatal(err)
		}
		out, err = run("get", "HR.P_OVC_E2E")
		t.Logf("ovc get after a hand edit:\n%s", out)
		b, _ := os.ReadFile(filepath.Join(work, "HR/procedures/P_OVC_E2E.prc"))
		if err != nil || !strings.Contains(out, "synced (edited on the database outside OVC") || !strings.Contains(string(b), "hotfix") {
			t.Errorf("sync: %v\n%s\nfile: %s", err, out, b)
		}
	}

	// unknown schema: no objects -> failure, nothing registered
	if _, err := run("init", "NOSUCH", "--db", "dev"); err == nil || !strings.Contains(err.Error(), "no manageable objects") {
		t.Errorf("unknown schema: %v", err)
	}
	if tree := git("ls-tree", "--name-only", "refs/heads/dev"); strings.Contains(tree, "NOSUCH") {
		t.Errorf("folder created for an unknown schema:\n%s", tree)
	}
}
