package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ovc/internal/config"
	"ovc/internal/gitops"
	"ovc/internal/jobs"
	"ovc/internal/layout"
	"ovc/internal/oracle/export"
	"ovc/internal/store"
)

type fakeCatalog struct {
	objs    []export.Object            // objects of every alias without an entry in byAlias
	byAlias map[string][]export.Object // per "alias/SCHEMA" or per alias (dev and prd differ)
	err     error
	block   chan struct{} // when set, Objects waits for it to be closed

	failNames map[string]bool   // Hydrate fails for these object names
	ddl       map[string]string // object name -> DDL now on the database (default: made up)
	hydrated  []string          // what Hydrate was asked for, "TYPE NAME"
}

// Hydrate returns "DDL of <TYPE> <NAME> from <alias>" for every object, or
// hydrateErr for the names listed in failNames.
func (f *fakeCatalog) Hydrate(ctx context.Context, alias, schema string, objs []export.Object, workers int) ([]export.File, error) {
	out := make([]export.File, len(objs))
	for i, o := range objs {
		out[i] = export.File{Object: o, Content: fmt.Sprintf("CREATE OR REPLACE -- DDL of %s %s from %s\n/\n", o.Type, o.Name, alias)}
		if c, ok := f.ddl[o.Name]; ok {
			out[i].Content = c
		}
		if f.failNames[o.Name] {
			out[i].Err = errors.New("ORA-31603: object not found")
		}
		f.hydrated = append(f.hydrated, string(o.Type)+" "+o.Name)
	}
	return out, nil
}

// "reporting" is a configured database that is not an env.
func (f *fakeCatalog) HasAlias(a string) bool { return a == "dev" || a == "prd" || a == "reporting" }
func (f *fakeCatalog) Objects(ctx context.Context, alias, schema string, exclude []string) ([]export.Object, error) {
	if f.block != nil {
		<-f.block
	}
	for _, k := range []string{alias + "/" + schema, alias} {
		if o, ok := f.byAlias[k]; ok {
			return o, f.err
		}
	}
	return f.objs, f.err
}

var ts = time.Date(2026, 10, 1, 10, 15, 0, 0, time.UTC)

func sampleObjects() []export.Object {
	return []export.Object{
		{Type: layout.PackageSpec, Name: "PKG_EMPLOYEE", LastDDLTime: ts},
		{Type: layout.PackageBody, Name: "PKG_EMPLOYEE", LastDDLTime: ts},
		{Type: layout.Table, Name: "EMPLOYEES", LastDDLTime: ts},
		{Type: layout.Function, Name: "KiemTra_MST", LastDDLTime: ts},
		{Type: layout.Function, Name: "KIEMTRA_MST", LastDDLTime: ts},
	}
}

// fakeHost records the pull requests OVC opens.
type fakeHost struct {
	mu  sync.Mutex
	prs []string // "source -> target: title"
	err error
}

func (f *fakeHost) CreateChangeRequest(_ context.Context, source, target, title, body string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	f.prs = append(f.prs, source+" -> "+target+": "+title+"\n"+body)
	return fmt.Sprintf("https://github.com/o/r/pull/%d", len(f.prs)), nil
}

type env struct {
	srv    *httptest.Server
	remote string
	mirror *gitops.Repo
	store  *store.File
	jobs   *jobs.Manager
	cat    *fakeCatalog
	host   *fakeHost
	// beforePush runs inside the server right before it pushes.
	beforePush func(branch string)
}

func newEnv(t *testing.T) *env {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	remote := filepath.Join(t.TempDir(), "remote.git")
	if out, err := exec.Command("git", "init", "--bare", "--quiet", remote).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	ctx := context.Background()
	mirror, err := gitops.Open(ctx, filepath.Join(t.TempDir(), "mirror.git"), gitops.Config{Remote: remote})
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenFile(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Server{Envs: []string{"dev", "prd"}}
	cfg.Git.Committer.Name, cfg.Git.Committer.Email = "OVC Bot", "ovc-bot@company.local"
	cfg.CI.GitHubTemplate = "org/ovc-ci-templates/.github/workflows/oracle-deploy.yml@v1"
	cfg.CI.GitLabProject = "db/ovc-ci-templates"
	cfg.CLI.MinVersion = "0.1.0"
	cat := &fakeCatalog{objs: sampleObjects()}
	jm := jobs.New()
	host := &fakeHost{}
	e := &env{remote: remote, mirror: mirror, store: st, jobs: jm, cat: cat, host: host}
	srv := httptest.NewServer(New(Deps{Cfg: cfg, Git: mirror, Store: st, Catalog: cat, Jobs: jm, Host: host,
		beforePush: func(b string) {
			if e.beforePush != nil {
				e.beforePush(b)
			}
		}}))
	e.srv = srv
	t.Cleanup(func() { jm.Wait(); srv.Close() })
	return e
}

// do sends a request with the default client headers; a header whose value in
// hdr is "-" is omitted.
func (e *env) do(t *testing.T, method, path string, body any, hdr map[string]string) (int, map[string]any) {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	h := map[string]string{"X-OVC-Client-Version": "0.1.0", "X-OVC-User-Name": "Nguyen Van A", "X-OVC-User-Email": "a@company.local"}
	for k, v := range hdr {
		h[k] = v
	}
	for k, v := range h {
		if v != "-" {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (e *env) waitJob(t *testing.T, id string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		_, j := e.do(t, "GET", "/api/v1/jobs/"+id, nil, nil)
		if j["status"] != "running" {
			return j
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("job did not finish")
	return nil
}

func (e *env) initSchema(t *testing.T, schema string) (int, map[string]any) {
	t.Helper()
	return e.initEnv(t, schema, "dev")
}

func (e *env) initEnv(t *testing.T, schema, db string) (int, map[string]any) {
	t.Helper()
	return e.do(t, "POST", "/api/v1/schemas", map[string]any{"schema": schema, "db_alias": db}, nil)
}

// mustInit runs an init to completion and returns the job result.
func (e *env) mustInit(t *testing.T, schema, envName string) map[string]any {
	t.Helper()
	code, body := e.initEnv(t, schema, envName)
	if code != 202 {
		t.Fatalf("POST %s %s = %d %v", schema, envName, code, body)
	}
	job := e.waitJob(t, body["job_id"].(string))
	if job["status"] != "succeeded" {
		t.Fatalf("init %s %s: %v", schema, envName, job)
	}
	return job["result"].(map[string]any)
}

func (e *env) remoteGit(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Env = append(os.Environ(), "GIT_DIR="+e.remote, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func errCode(out map[string]any) any {
	e, _ := out["error"].(map[string]any)
	return e["code"]
}

func TestHealthAndVersion(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/healthz", "/readyz", "/api/v1/version"} {
		resp, err := http.Get(e.srv.URL + p)
		if err != nil || resp.StatusCode != 200 {
			t.Errorf("%s: %v %v", p, resp, err)
		}
	}
}

func TestClientHeaders(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct {
		name string
		hdr  map[string]string
		want int
		code string
	}{
		{"no version", map[string]string{"X-OVC-Client-Version": "-"}, 400, "CLIENT_VERSION_REQUIRED"},
		{"too old", map[string]string{"X-OVC-Client-Version": "0.0.9"}, 426, "CLIENT_TOO_OLD"},
		{"dev suffix is fine", map[string]string{"X-OVC-Client-Version": "0.1.0-dev"}, 200, ""},
		{"no name", map[string]string{"X-OVC-User-Name": "-"}, 400, "IDENTITY_REQUIRED"},
		{"bad email", map[string]string{"X-OVC-User-Email": "nobody"}, 400, "IDENTITY_REQUIRED"},
		{"injected brackets", map[string]string{"X-OVC-User-Name": "A <evil@x>"}, 400, "IDENTITY_REQUIRED"},
	} {
		status, out := e.do(t, "GET", "/api/v1/schemas", nil, c.hdr)
		if status != c.want {
			t.Errorf("%s: status %d, want %d", c.name, status, c.want)
		}
		if c.code != "" && errCode(out) != c.code {
			t.Errorf("%s: code %v, want %s", c.name, errCode(out), c.code)
		}
	}
}

func (e *env) tree(t *testing.T, ref string) []string {
	t.Helper()
	return strings.Split(e.remoteGit(t, "ls-tree", "-r", "--name-only", ref), "\n")
}

func TestInitSchemaHappyPath(t *testing.T) {
	e := newEnv(t)
	code, body := e.initSchema(t, "qlsc") // lower case means the upper-case owner
	if code != 202 {
		t.Fatalf("POST = %d %v", code, body)
	}
	job := e.waitJob(t, body["job_id"].(string))
	if job["status"] != "succeeded" {
		t.Fatalf("job = %v", job)
	}
	res := job["result"].(map[string]any)
	if res["objects"].(float64) != 5 || res["branch"] != "dev" || res["dir"] != "QLSC" ||
		res["first_db"] != true || res["new_branch"] != true || res["schema"] != "QLSC" {
		t.Errorf("result = %v", res)
	}

	// only the branch of the database that was initialised exists on the REMOTE
	if refs := e.remoteGit(t, "for-each-ref", "--format=%(refname)", "refs/heads"); refs != "refs/heads/dev" {
		t.Errorf("remote branches = %q, want only dev", refs)
	}
	// history: the owner baseline (root, only QLSC/), then the CI files at the root
	if e.remoteGit(t, "rev-parse", "dev") != res["commit"] || e.remoteGit(t, "rev-parse", "dev~1") != res["baseline"] {
		t.Errorf("dev history does not match result %v", res)
	}
	if base := e.tree(t, res["baseline"].(string)); len(base) != 6 || !strings.HasPrefix(base[0], "QLSC/") {
		t.Errorf("baseline must hold only QLSC/: %v", base)
	}
	want := []string{
		"QLSC/packages/PKG_EMPLOYEE.pks", "QLSC/packages/PKG_EMPLOYEE.pkb", "QLSC/tables/EMPLOYEES.sql",
		"QLSC/functions/K~iem~T~ra_~MST.fnc", "QLSC/functions/KIEMTRA_MST.fnc", "QLSC/ovc.yaml",
		".github/workflows/ovc-deploy.yml", ".gitlab-ci.yml",
	}
	tree := e.tree(t, "dev")
	have := map[string]bool{}
	for _, f := range tree {
		have[f] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("missing %s in %v", w, tree)
		}
	}
	if len(tree) != len(want) {
		t.Errorf("tree has %d files, want %d: %v", len(tree), len(want), tree)
	}
	if stub := e.remoteGit(t, "show", "dev:QLSC/packages/PKG_EMPLOYEE.pkb"); stub != "-- OVC:STUB type=PACKAGE_BODY" {
		t.Errorf("stub = %q", stub)
	}
	if y := e.remoteGit(t, "show", "dev:QLSC/ovc.yaml"); !strings.Contains(y, "schema: QLSC") || !strings.Contains(y, "envs:") {
		t.Errorf("ovc.yaml = %s", y)
	}
	if g := e.remoteGit(t, "show", "dev:.github/workflows/ovc-deploy.yml"); !strings.Contains(g, `["dev", "prd"]`) || !strings.Contains(g, "uses: org/ovc-ci-templates") {
		t.Errorf("github workflow = %s", g)
	}
	// authorship: declared user as author, bot as committer, trailers for audit
	log := e.remoteGit(t, "log", "--format=%an <%ae>|%cn <%ce>|%s", "dev")
	if log != "Nguyen Van A <a@company.local>|OVC Bot <ovc-bot@company.local>|ovc: CI files of database branch dev\n"+
		"Nguyen Van A <a@company.local>|OVC Bot <ovc-bot@company.local>|ovc init QLSC --db dev: baseline of QLSC (5 objects)" {
		t.Errorf("log = %q", log)
	}
	if tr := e.remoteGit(t, "log", "-1", "--format=%(trailers:key=OVC-Init,valueonly)", "dev~1"); tr != "QLSC db=dev" {
		t.Errorf("OVC-Init trailer = %q", tr)
	}
	// every object path round-trips through layout
	for _, f := range tree {
		owner, rel, ok := layout.SplitOwner(f)
		if !ok {
			continue
		}
		if owner != "QLSC" {
			t.Errorf("%s: owner %q", f, owner)
		}
		if _, err := layout.Parse(rel); err != nil && rel != "ovc.yaml" {
			t.Errorf("layout.Parse(%s): %v", rel, err)
		}
	}
	// last_ddl_time lives on the server, per owner and database
	times, err := e.store.DDLTimes("QLSC", "dev")
	if err != nil || len(times) != 5 || !times["QLSC/packages/PKG_EMPLOYEE.pkb"].Equal(ts) {
		t.Errorf("ddl times = %v, %v", times, err)
	}
	_, list := e.do(t, "GET", "/api/v1/schemas", nil, nil)
	schemas := list["schemas"].([]any)
	if len(schemas) != 1 || schemas[0].(map[string]any)["name"] != "QLSC" {
		t.Errorf("list = %v", list)
	}
	if dbs := schemas[0].(map[string]any)["dbs"].(map[string]any); len(dbs) != 1 || dbs["dev"] == nil {
		t.Errorf("dbs = %v", dbs)
	}
}

// Two owners on one database share its branch: the second one is merged in as
// a folder without touching the first one's files.
func TestInitTwoOwnersShareTheDBBranch(t *testing.T) {
	e := newEnv(t)
	e.cat.byAlias = map[string][]export.Object{
		"dev/QLSC": {{Type: layout.Table, Name: "T_QLSC", LastDDLTime: ts}},
	}
	e.mustInit(t, "HR", "dev")
	before := e.remoteGit(t, "rev-parse", "dev")
	hrTree := e.remoteGit(t, "rev-parse", "dev:HR")
	res := e.mustInit(t, "QLSC", "dev")
	if res["new_branch"] != false || res["first_db"] != true {
		t.Errorf("result = %v", res)
	}
	if p := e.remoteGit(t, "log", "-1", "--format=%P|%s", "dev"); p != before+" "+res["baseline"].(string)+"|ovc init QLSC --db dev: add QLSC/ (1 objects)" {
		t.Errorf("merge commit = %q", p)
	}
	if e.remoteGit(t, "rev-parse", "dev:HR") != hrTree {
		t.Error("HR/ changed when QLSC was added")
	}
	if !strings.Contains(e.remoteGit(t, "ls-tree", "-r", "--name-only", "dev"), "QLSC/tables/T_QLSC.sql") {
		t.Error("QLSC/ missing")
	}
	if len(e.store.List()) != 2 {
		t.Errorf("registry = %v", e.store.List())
	}
}

// A later database reads its own DB and merges the same owner baseline: the
// owner's files share an ancestor on both branches, differ only by the
// objects each DB has, and a change made on dev merges into prd cleanly.
func TestInitSecondDB(t *testing.T) {
	e := newEnv(t)
	prdTime := ts.Add(48 * time.Hour) // a different DB compiles at different times
	e.cat.byAlias = map[string][]export.Object{
		"dev": sampleObjects(),
		"prd": {
			{Type: layout.PackageSpec, Name: "PKG_EMPLOYEE", LastDDLTime: prdTime},
			{Type: layout.PackageBody, Name: "PKG_EMPLOYEE", LastDDLTime: prdTime},
			{Type: layout.Table, Name: "EMPLOYEES", LastDDLTime: prdTime},
			{Type: layout.Function, Name: "KiemTra_MST", LastDDLTime: prdTime},
			// KIEMTRA_MST exists only on dev; P_PRD_ONLY only on prd
			{Type: layout.Procedure, Name: "P_PRD_ONLY", LastDDLTime: prdTime},
		},
		"prd/OTHER": {{Type: layout.Table, Name: "X", LastDDLTime: prdTime}},
	}
	devRes := e.mustInit(t, "QLSC", "dev")
	e.mustInit(t, "OTHER", "prd") // prd already exists with another owner
	prdRes := e.mustInit(t, "QLSC", "prd")
	baseline := devRes["baseline"].(string)
	if prdRes["first_db"] == true || prdRes["new_branch"] == true || prdRes["baseline"] != baseline ||
		prdRes["added"].(float64) != 1 || prdRes["removed"].(float64) != 1 {
		t.Errorf("prd result = %v", prdRes)
	}
	// prd: ... -> merge of the baseline -> align commit
	if p := e.remoteGit(t, "log", "-1", "--format=%P", "prd~1"); !strings.HasSuffix(p, " "+baseline) {
		t.Errorf("prd~1 must merge the owner baseline, parents = %q", p)
	}
	if mb := e.remoteGit(t, "merge-base", "dev", "prd"); mb != baseline {
		t.Errorf("merge-base = %s, want the owner baseline %s", mb, baseline)
	}
	if d := e.remoteGit(t, "diff", "--name-status", baseline, "prd", "--", "QLSC"); d != "D\tQLSC/functions/KIEMTRA_MST.fnc\nA\tQLSC/procedures/P_PRD_ONLY.prc" {
		t.Errorf("prd QLSC/ differs from baseline by:\n%s", d)
	}
	if tr := e.remoteGit(t, "log", "-1", "--format=%s|%(trailers:key=OVC-Init,valueonly)", "prd"); tr != "ovc init QLSC --db prd: align QLSC/ with db prd (+1 -1 objects)|QLSC db=prd" {
		t.Errorf("prd commit = %q", tr)
	}
	times, err := e.store.DDLTimes("QLSC", "prd")
	if err != nil || !times["QLSC/procedures/P_PRD_ONLY.prc"].Equal(prdTime) || !times["QLSC/tables/EMPLOYEES.sql"].Equal(prdTime) {
		t.Errorf("prd ddl times = %v, %v", times, err)
	}
	sc, _ := e.store.Get("QLSC")
	if len(sc.DBs) != 2 || sc.DBs["prd"].Branch != "prd" || sc.DBs["dev"].Branch != "dev" || sc.Dir != "QLSC" {
		t.Errorf("registry = %+v", sc)
	}

	// hydrate + edit on dev only, then merge dev -> prd: must be clean
	id := gitops.Identity{Name: "x", Email: "x@x"}
	ctx := context.Background()
	if _, err := e.mirror.Commit(ctx, "dev", "", []gitops.Change{{Path: "QLSC/packages/PKG_EMPLOYEE.pkb",
		Content: []byte("CREATE OR REPLACE PACKAGE BODY PKG_EMPLOYEE AS\nEND;\n/\n")}},
		gitops.CommitOpts{Author: id, Committer: id, Message: "hydrate + edit"}); err != nil {
		t.Fatal(err)
	}
	if err := e.mirror.Push(ctx, "dev"); err != nil {
		t.Fatal(err)
	}
	e.remoteGit(t, "merge-tree", "--write-tree", "prd", "dev") // fails the test on conflict

	if c, b := e.initEnv(t, "qlsc", "prd"); c != 409 || errCode(b) != "ALREADY_INITIALIZED" {
		t.Errorf("prd again = %d %v", c, b)
	}
}

func TestInitRejections(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct {
		name string
		body any
		code string
	}{
		{"bad schema", map[string]any{"schema": "bad name", "db_alias": "dev"}, "INVALID_SCHEMA"},
		{"unknown alias", map[string]any{"schema": "HR", "db_alias": "nope"}, "UNKNOWN_DB_ALIAS"},
		{"alias that is not a db branch", map[string]any{"schema": "HR", "db_alias": "reporting"}, "INVALID_DB_BRANCH"},
		{"envs is gone: one init per db", map[string]any{"schema": "HR", "db_alias": "dev", "envs": []string{"dev", "prd"}}, "INVALID_REQUEST"},
	} {
		status, out := e.do(t, "POST", "/api/v1/schemas", c.body, nil)
		if status != 400 || errCode(out) != c.code {
			t.Errorf("%s: %d %v, want 400 %s", c.name, status, out, c.code)
		}
	}
}

func TestInitDuplicateAndConcurrent(t *testing.T) {
	e := newEnv(t)
	e.cat.block = make(chan struct{})
	c1, b1 := e.initSchema(t, "HR")
	c2, b2 := e.initSchema(t, "hr") // same schema, different case, while the first is running
	if c1 != 202 {
		t.Fatalf("first = %d %v", c1, b1)
	}
	if c2 != 409 || errCode(b2) != "INIT_IN_PROGRESS" {
		t.Errorf("second = %d %v", c2, b2)
	}
	close(e.cat.block)
	if j := e.waitJob(t, b1["job_id"].(string)); j["status"] != "succeeded" {
		t.Fatalf("job = %v", j)
	}
	c3, b3 := e.initSchema(t, "Hr")
	if c3 != 409 || errCode(b3) != "ALREADY_INITIALIZED" {
		t.Errorf("third = %d %v", c3, b3)
	}
}

func TestInitFailuresLeaveNothingBehind(t *testing.T) {
	failedOn := func(t *testing.T, e *env, db, wantErr string) {
		t.Helper()
		_, b := e.initEnv(t, "HR", db)
		j := e.waitJob(t, b["job_id"].(string))
		if j["status"] != "failed" || !strings.Contains(j["error"].(string), wantErr) {
			t.Fatalf("job = %v, want failure containing %q", j, wantErr)
		}
		if sc, ok := e.store.Get("HR"); ok && sc.DBs[db].Branch != "" {
			t.Errorf("HR on %s must not be registered", db)
		}
	}
	failed := func(t *testing.T, e *env, wantErr string) { t.Helper(); failedOn(t, e, "dev", wantErr) }
	t.Run("no objects", func(t *testing.T) {
		e := newEnv(t)
		e.cat.objs = nil
		failed(t, e, "no manageable objects")
	})
	t.Run("db error", func(t *testing.T) {
		e := newEnv(t)
		e.cat.err = errors.New("ORA-01017: invalid credential")
		failed(t, e, "ORA-01017")
	})
	t.Run("folder already on the branch but not registered", func(t *testing.T) {
		e := newEnv(t)
		seed, _ := gitops.Open(context.Background(), filepath.Join(t.TempDir(), "s.git"), gitops.Config{Remote: e.remote})
		id := gitops.Identity{Name: "x", Email: "x@x"}
		seed.CreateBranch(context.Background(), "dev", map[string][]byte{"HR/a.sql": []byte("1")}, gitops.CommitOpts{Author: id, Committer: id, Message: "m"})
		if err := seed.Push(context.Background(), "dev"); err != nil {
			t.Fatal(err)
		}
		before := e.remoteGit(t, "rev-parse", "dev")
		failed(t, e, "refusing to overwrite")
		if after := e.remoteGit(t, "rev-parse", "dev"); after != before {
			t.Error("an existing remote branch was modified")
		}
	})
	t.Run("push rejected: a new branch is removed", func(t *testing.T) {
		e := newEnv(t)
		hook := filepath.Join(e.remote, "hooks", "pre-receive")
		os.MkdirAll(filepath.Dir(hook), 0o755)
		os.WriteFile(hook, []byte("#!/bin/sh\necho protected >&2\nexit 1\n"), 0o755)
		failed(t, e, "rolled back")
		if refs := e.remoteGit(t, "for-each-ref", "refs/heads"); refs != "" {
			t.Errorf("remote has branches after rollback:\n%s", refs)
		}
		if _, err := e.mirror.Head(context.Background(), "dev"); err == nil {
			t.Error("local mirror still has dev")
		}
		os.Remove(hook)
		e.mustInit(t, "HR", "dev")
	})
	t.Run("push rejected: an existing branch is left as it was", func(t *testing.T) {
		e := newEnv(t)
		e.cat.byAlias = map[string][]export.Object{"prd/OTHER": {{Type: layout.Table, Name: "X", LastDDLTime: ts}}}
		e.mustInit(t, "HR", "dev")
		e.mustInit(t, "OTHER", "prd")
		prdHead := e.remoteGit(t, "rev-parse", "prd")
		hook := filepath.Join(e.remote, "hooks", "pre-receive")
		os.MkdirAll(filepath.Dir(hook), 0o755)
		os.WriteFile(hook, []byte("#!/bin/sh\necho protected >&2\nexit 1\n"), 0o755)
		failedOn(t, e, "prd", "rolled back")
		if h := e.remoteGit(t, "rev-parse", "prd"); h != prdHead {
			t.Errorf("remote prd moved: %s -> %s", prdHead, h)
		}
		if h, _ := e.mirror.Head(context.Background(), "prd"); h != prdHead {
			t.Errorf("local prd = %s, want %s", h, prdHead)
		}
		os.Remove(hook)
		e.mustInit(t, "HR", "prd")
	})
}

// Someone else moves the database branch between our fetch and our push (a
// PR of another owner merged meanwhile): the init redoes its commits on the
// new head instead of failing or overwriting.
func TestInitRedoesWhenTheBranchMoves(t *testing.T) {
	e := newEnv(t)
	e.mustInit(t, "OTHER", "dev")
	seed, _ := gitops.Open(context.Background(), filepath.Join(t.TempDir(), "s.git"), gitops.Config{Remote: e.remote})
	id := gitops.Identity{Name: "x", Email: "x@x"}
	pushes := 0
	e.beforePush = func(b string) {
		pushes++
		if pushes > 1 {
			return
		}
		ctx := context.Background()
		seed.Fetch(ctx)
		if _, err := seed.Commit(ctx, "dev", "", []gitops.Change{{Path: "OTHER/tables/NEW.sql", Content: []byte("x")}},
			gitops.CommitOpts{Author: id, Committer: id, Message: "merged PR"}); err != nil {
			t.Error(err)
		}
		if err := seed.Push(ctx, "dev"); err != nil {
			t.Error(err)
		}
	}
	res := e.mustInit(t, "HR", "dev")
	if pushes != 2 {
		t.Errorf("pushes = %d, want 2 (one rejected, one redone)", pushes)
	}
	tree := e.remoteGit(t, "ls-tree", "-r", "--name-only", "dev")
	if !strings.Contains(tree, "OTHER/tables/NEW.sql") || !strings.Contains(tree, "HR/ovc.yaml") {
		t.Errorf("dev lost a commit:\n%s", tree)
	}
	if e.remoteGit(t, "rev-parse", "dev") != res["commit"] {
		t.Error("result commit is not the remote head")
	}
}

func (e *env) hydrate(t *testing.T, schema string, body map[string]any) map[string]any {
	t.Helper()
	code, out := e.do(t, "POST", "/api/v1/schemas/"+schema+"/hydrate", body, nil)
	if code != 202 {
		t.Fatalf("hydrate = %d %v", code, out)
	}
	j := e.waitJob(t, out["job_id"].(string))
	if j["status"] != "succeeded" {
		t.Fatalf("hydrate job = %v", j)
	}
	return j["result"].(map[string]any)
}

func strs(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

func TestHydrate(t *testing.T) {
	e := newEnv(t)
	objs := append(sampleObjects(),
		export.Object{Type: layout.Index, Name: "EMPLOYEES_IX1", Parent: "EMPLOYEES", LastDDLTime: ts},
		export.Object{Type: layout.Constraint, Name: "EMPLOYEES", LastDDLTime: ts},
		export.Object{Type: layout.Procedure, Name: "P_GONE", LastDDLTime: ts},
		export.Object{Type: layout.Procedure, Name: "P_BROKEN", LastDDLTime: ts},
	)
	e.cat.objs = objs
	e.mustInit(t, "HR", "dev")
	before := e.remoteGit(t, "rev-parse", "dev")

	// P_GONE is dropped and P_KIEMTRA recompiled after init
	e.cat.objs = nil
	for _, o := range objs {
		switch o.Name {
		case "P_GONE":
			continue
		case "KiemTra_MST":
			o.LastDDLTime = ts.Add(time.Hour)
		}
		e.cat.objs = append(e.cat.objs, o)
	}
	e.cat.failNames = map[string]bool{"P_BROKEN": true}

	res := e.hydrate(t, "hr", map[string]any{"db": "dev", "paths": []string{
		"packages/PKG_EMPLOYEE.pkb", "tables/EMPLOYEES.sql", "functions/K~iem~T~ra_~MST.fnc",
		"procedures/P_GONE.prc", "procedures/P_BROKEN.prc", "nope/X.sql"}})
	got := strs(res["hydrated"])
	want := []string{"constraints/EMPLOYEES.sql", "functions/K~iem~T~ra_~MST.fnc", "indexes/EMPLOYEES_IX1.sql", "packages/PKG_EMPLOYEE.pkb", "tables/EMPLOYEES.sql"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("hydrated = %v, want %v (a table brings its index and FKs)", got, want)
	}
	failed := fmt.Sprint(res["failed"])
	for _, f := range []string{"P_GONE.prc", "not in db dev", "P_BROKEN.prc", "ORA-31603", "nope/X.sql"} {
		if !strings.Contains(failed, f) {
			t.Errorf("failed lacks %q: %s", f, failed)
		}
	}
	if w := fmt.Sprint(res["warnings"]); !strings.Contains(w, "K~iem~T~ra_~MST.fnc changed on db dev since init") {
		t.Errorf("warnings = %s", w)
	}
	// one commit on dev only, real content in place, other files untouched
	if e.remoteGit(t, "rev-parse", "dev~1") != before || e.remoteGit(t, "rev-parse", "dev") != res["commit"] {
		t.Error("expected exactly one commit on dev")
	}
	if c := e.remoteGit(t, "show", "dev:HR/packages/PKG_EMPLOYEE.pkb"); c != "CREATE OR REPLACE -- DDL of PACKAGE BODY PKG_EMPLOYEE from dev\n/" {
		t.Errorf("content = %q", c)
	}
	if c := e.remoteGit(t, "show", "dev:HR/packages/PKG_EMPLOYEE.pks"); c != "-- OVC:STUB type=PACKAGE" {
		t.Errorf("spec must stay a stub: %q", c)
	}
	if tr := e.remoteGit(t, "log", "-1", "--format=%an|%cn|%(trailers:key=OVC-Hydrate,valueonly)", "dev"); tr != "Nguyen Van A|OVC Bot|HR db=dev objects=5" {
		t.Errorf("commit = %q", tr)
	}

	// again: nothing left to do for those, no new commit
	head := e.remoteGit(t, "rev-parse", "dev")
	res = e.hydrate(t, "HR", map[string]any{"paths": []string{"packages/PKG_EMPLOYEE.pkb"}})
	if len(strs(res["hydrated"])) != 0 || strings.Join(strs(res["up_to_date"]), ",") != "packages/PKG_EMPLOYEE.pkb" || res["commit"] != head {
		t.Errorf("second hydrate = %v", res)
	}

	// all stubs of one type, then everything
	e.cat.failNames = nil
	res = e.hydrate(t, "HR", map[string]any{"all": true, "types": []string{"package"}})
	if strings.Join(strs(res["hydrated"]), ",") != "packages/PKG_EMPLOYEE.pks" {
		t.Errorf("all packages = %v", res["hydrated"])
	}
	res = e.hydrate(t, "HR", map[string]any{"all": true})
	if strings.Join(strs(res["hydrated"]), ",") != "functions/KIEMTRA_MST.fnc,procedures/P_BROKEN.prc" {
		t.Errorf("all = %v (P_GONE has no object any more)", res["hydrated"])
	}
	if !strings.Contains(fmt.Sprint(res["failed"]), "P_GONE") {
		t.Errorf("failed = %v", res["failed"])
	}
	if n := len(strs(res["up_to_date"])); n != 6 {
		t.Errorf("all: %d up to date, want the 6 hydrated before: %v", n, res["up_to_date"])
	}
	if tree := e.remoteGit(t, "ls-tree", "-r", "--name-only", "dev"); !strings.Contains(tree, "HR/ovc.yaml") {
		t.Error("ovc.yaml must stay")
	}
}

// The three-way decision of spec §9.8 step 3 for an object that has content:
// base (last agreement) / database / branch.
func TestGetSyncsTheDatabaseAndOpensConflicts(t *testing.T) {
	e := newEnv(t)
	e.mustInit(t, "HR", "dev")
	const p = "procedures/P_SYNC.prc"
	e.cat.objs = append(sampleObjects(), export.Object{Type: layout.Procedure, Name: "P_SYNC", LastDDLTime: ts})
	// P_SYNC appears on the db after init: add its stub the way a later init would
	e.cat.ddl = map[string]string{"P_SYNC": "CREATE OR REPLACE PROCEDURE P_SYNC IS BEGIN v1; END;\n/\n"}
	seed, _ := gitops.Open(context.Background(), filepath.Join(t.TempDir(), "s.git"), gitops.Config{Remote: e.remote})
	bob := gitops.Identity{Name: "Bob", Email: "bob@company.local"}
	push := func(content string) {
		t.Helper()
		ctx := context.Background()
		if err := seed.Fetch(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := seed.Commit(ctx, "dev", "", []gitops.Change{{Path: "HR/" + p, Content: []byte(content)}},
			gitops.CommitOpts{Author: bob, Committer: bob, Message: "merged MR"}); err != nil {
			t.Fatal(err)
		}
		if err := seed.Push(ctx, "dev"); err != nil {
			t.Fatal(err)
		}
	}
	push("-- OVC:STUB type=PROCEDURE\n")
	get := func() map[string]any { t.Helper(); return e.hydrate(t, "HR", map[string]any{"paths": []string{p}}) }
	show := func() string { return e.remoteGit(t, "show", "dev:HR/"+p) + "\n" }

	res := get() // stub -> hydrate, base recorded
	if strings.Join(strs(res["hydrated"]), ",") != p {
		t.Fatalf("hydrate = %v", res)
	}

	res = get() // nothing changed anywhere
	if strings.Join(strs(res["up_to_date"]), ",") != p {
		t.Errorf("up to date = %v", res)
	}

	// edited by hand on the database: synced onto the branch
	e.cat.ddl["P_SYNC"] = "CREATE OR REPLACE PROCEDURE P_SYNC IS BEGIN hotfix; END;\n/\n"
	res = get()
	if strings.Join(strs(res["synced"]), ",") != p || show() != e.cat.ddl["P_SYNC"] {
		t.Errorf("sync = %v, branch has %q", res, show())
	}
	if tr := e.remoteGit(t, "log", "-1", "--format=%(trailers:key=OVC-Sync,valueonly)", "dev"); tr != "HR db=dev objects=1" {
		t.Errorf("OVC-Sync trailer = %q", tr)
	}

	// a merged MR changes it on the branch, not deployed yet: never overwritten
	push("CREATE OR REPLACE PROCEDURE P_SYNC IS BEGIN new_code; END;\n/\n")
	head := e.remoteGit(t, "rev-parse", "dev")
	res = get()
	if strings.Join(strs(res["pending"]), ",") != p || e.remoteGit(t, "rev-parse", "dev") != head {
		t.Errorf("pending = %v", res)
	}

	// and now the database is edited by hand too: conflict, nothing written to dev
	e.cat.ddl["P_SYNC"] = "CREATE OR REPLACE PROCEDURE P_SYNC IS BEGIN hotfix2; END;\n/\n"
	res = get()
	if e.remoteGit(t, "rev-parse", "dev") != head {
		t.Error("a conflict must not write to dev")
	}
	cs := res["conflicts"].([]any)
	if len(cs) != 1 {
		t.Fatalf("conflicts = %v", res)
	}
	c := cs[0].(map[string]any)
	br := c["branch"].(string)
	if !strings.HasPrefix(br, "sync/dev/HR/procedures/P_SYNC.prc-") || c["url"] != "https://github.com/o/r/pull/1" || c["existing"] != false {
		t.Errorf("conflict = %v", c)
	}
	if fmt.Sprint(c["authors"]) != "[Bob <bob@company.local>]" {
		t.Errorf("who should resolve = %v", c["authors"])
	}
	if got := e.remoteGit(t, "show", br+":HR/"+p) + "\n"; got != e.cat.ddl["P_SYNC"] {
		t.Errorf("sync branch holds %q", got)
	}
	// made from the base (the sync commit), so the MR shows the conflict
	if e.remoteGit(t, "rev-parse", br+"~1") != e.remoteGit(t, "rev-parse", "dev~1") {
		t.Error("the sync branch must start at the object's base")
	}
	mt := exec.Command("git", "merge-tree", "--write-tree", "--name-only", "dev", br)
	mt.Env = append(os.Environ(), "GIT_DIR="+e.remote)
	if out, err := mt.CombinedOutput(); err == nil || !strings.Contains(string(out), "CONFLICT") {
		t.Errorf("the MR must show the conflict: %v\n%s", err, out)
	}
	if pr := e.host.prs[0]; !strings.HasPrefix(pr, br+" -> dev: OVC sync: HR/"+p) || !strings.Contains(pr, "Bob <bob@company.local>") {
		t.Errorf("pull request = %s", pr)
	}

	// asked again while the sync branch is open: reported, not duplicated
	res = get()
	c = res["conflicts"].([]any)[0].(map[string]any)
	if c["branch"] != br || c["existing"] != true || len(e.host.prs) != 1 {
		t.Errorf("second conflict = %v (prs %d)", c, len(e.host.prs))
	}

	// resolved: the MR merged (keeping both), branch deleted, then deployed
	e.mirror.DeleteBranch(context.Background(), br, true)
	resolved := "CREATE OR REPLACE PROCEDURE P_SYNC IS BEGIN new_code; hotfix2; END;\n/\n"
	push(resolved)
	e.cat.ddl["P_SYNC"] = resolved
	res = get()
	if strings.Join(strs(res["up_to_date"]), ",") != p || len(res["conflicts"].([]any)) != 0 {
		t.Errorf("after resolving = %v", res)
	}
}

func TestGetConflictWithoutABase(t *testing.T) {
	e := newEnv(t)
	e.mustInit(t, "HR", "dev")
	// content written on the branch without OVC (no base), different from the db
	seed, _ := gitops.Open(context.Background(), filepath.Join(t.TempDir(), "s.git"), gitops.Config{Remote: e.remote})
	seed.Fetch(context.Background())
	id := gitops.Identity{Name: "x", Email: "x@x"}
	seed.Commit(context.Background(), "dev", "", []gitops.Change{{Path: "HR/packages/PKG_EMPLOYEE.pkb", Content: []byte("by hand\n")}},
		gitops.CommitOpts{Author: id, Committer: id, Message: "m"})
	seed.Push(context.Background(), "dev")
	e.host.err = errors.New("github: HTTP 403")
	res := e.hydrate(t, "HR", map[string]any{"paths": []string{"packages/PKG_EMPLOYEE.pkb"}})
	cs := res["conflicts"].([]any)
	if len(cs) != 1 {
		t.Fatalf("result = %v", res)
	}
	c := cs[0].(map[string]any)
	if !strings.Contains(c["reason"].(string), "no record") || c["error"] != "github: HTTP 403" || c["branch"] == "" {
		t.Errorf("conflict = %v", c)
	}
	// the branch is pushed even when the MR could not be opened
	if refs := e.remoteGit(t, "for-each-ref", "--format=%(refname:short)", "refs/heads/sync"); !strings.HasPrefix(refs, "sync/dev/HR/packages/PKG_EMPLOYEE.pkb-") {
		t.Errorf("sync branches = %q", refs)
	}
}

func TestSyncBranchPrefix(t *testing.T) {
	a := syncBranchPrefix("dev", "HR", "functions/K~iem~T~ra_~MST.fnc")
	b := syncBranchPrefix("dev", "HR", "functions/K_iem_T_ra__MST.fnc")
	if a == b || !strings.HasPrefix(a, "sync/dev/HR/functions/K_iem_T_ra__MST.fnc-") || strings.Contains(a, "~") {
		t.Errorf("prefixes %q %q", a, b)
	}
	if out, err := exec.Command("git", "check-ref-format", "refs/heads/"+a+"20261007T153000").CombinedOutput(); err != nil {
		t.Errorf("not a valid ref name: %s %v", out, err)
	}
}

func TestHydrateRejections(t *testing.T) {
	e := newEnv(t)
	e.mustInit(t, "HR", "dev")
	for _, c := range []struct {
		schema string
		body   map[string]any
		status int
		code   string
	}{
		{"HR", map[string]any{"db": "dev"}, 400, "INVALID_REQUEST"},
		{"HR", map[string]any{"paths": []string{"../x"}}, 400, "INVALID_PATH"},
		{"HR", map[string]any{"all": true, "types": []string{"nope"}}, 400, "INVALID_TYPE"},
		{"HR", map[string]any{"db": "prd", "all": true}, 404, "DB_NOT_INITIALIZED"},
		{"NOPE", map[string]any{"all": true}, 404, "SCHEMA_NOT_FOUND"},
	} {
		status, out := e.do(t, "POST", "/api/v1/schemas/"+c.schema+"/hydrate", c.body, nil)
		if status != c.status || errCode(out) != c.code {
			t.Errorf("%v: %d %v, want %d %s", c.body, status, out, c.status, c.code)
		}
	}
}

func TestJobNotFound(t *testing.T) {
	e := newEnv(t)
	code, out := e.do(t, "GET", "/api/v1/jobs/deadbeef", nil, nil)
	if code != 404 || errCode(out) != "JOB_NOT_FOUND" {
		t.Errorf("%d %v", code, out)
	}
}

func TestCompareVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"0.1.0", "0.1.0", 0}, {"0.1.0-dev", "0.1.0", 0}, {"v0.2.0", "0.1.9", 1},
		{"0.0.9", "0.1.0", -1}, {"1.0.0", "0.99.99", 1}, {"0.1", "0.1.0", 0},
	} {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compare(%s,%s) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
