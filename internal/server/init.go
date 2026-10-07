package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"ovc/internal/ci"
	"ovc/internal/gitops"
	"ovc/internal/jobs"
	"ovc/internal/layout"
	"ovc/internal/oracle/export"
	"ovc/internal/store"
)

type initSet struct {
	mu sync.Mutex
	m  map[string]bool
}

func newInitSet() initSet { return initSet{m: map[string]bool{}} }

func (s *initSet) acquire(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := strings.ToLower(name)
	if s.m[k] {
		return false
	}
	s.m[k] = true
	return true
}

func (s *initSet) release(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, strings.ToLower(name))
}

type initRequest struct {
	Schema  string `json:"schema"`
	DBAlias string `json:"db_alias"` // also the branch: init --db dev writes to branch dev
}

var schemaNameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_$#]*$`)

// InitResult is the job result of an init (spec §9.1).
type InitResult struct {
	Schema     string         `json:"schema"`
	DB         string         `json:"db"`
	Branch     string         `json:"branch"`
	Dir        string         `json:"dir"` // the owner's folder on the branch
	Objects    int            `json:"objects"`
	ByType     map[string]int `json:"by_type"`
	Commit     string         `json:"commit"`   // head of the branch after init
	Baseline   string         `json:"baseline"` // owner baseline
	FirstDB    bool           `json:"first_db"` // this init created the owner baseline
	NewBranch  bool           `json:"new_branch"`
	Added      int            `json:"added,omitempty"`   // later dbs: stubs only this db has
	Removed    int            `json:"removed,omitempty"` // later dbs: stubs this db lacks
	Warnings   []string       `json:"warnings,omitempty"`
	FilesCount int            `json:"files"` // files in the owner's folder
}

// POST /api/v1/schemas
func (s *Server) handleInitSchema(w http.ResponseWriter, r *http.Request) {
	var req initRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, &apiError{400, "INVALID_REQUEST", "body must be JSON {schema, db_alias}: " + err.Error(), nil})
		return
	}
	req.Schema = strings.TrimSpace(req.Schema)
	if !schemaNameRe.MatchString(req.Schema) {
		writeError(w, &apiError{400, "INVALID_SCHEMA", fmt.Sprintf("schema %q is not a plain Oracle name", req.Schema), nil})
		return
	}
	// Owners are stored upper case unless created quoted; a plain name typed
	// in lower case means the upper-case owner, as in SQL.
	owner := strings.ToUpper(req.Schema)
	dir, err := layout.OwnerDir(owner)
	if err != nil {
		writeError(w, &apiError{400, "INVALID_SCHEMA", err.Error(), nil})
		return
	}
	if !s.Catalog.HasAlias(req.DBAlias) {
		writeError(w, &apiError{400, "UNKNOWN_DB_ALIAS", fmt.Sprintf("db alias %q is not configured", req.DBAlias), nil})
		return
	}
	branch, err := layout.DBBranch(req.DBAlias)
	if err != nil || !slices.Contains(s.Cfg.Envs, req.DBAlias) {
		writeError(w, &apiError{400, "INVALID_DB_BRANCH", fmt.Sprintf("db alias %q is not a database branch (have %v)", req.DBAlias, s.Cfg.Envs), nil})
		return
	}
	if sc, ok := s.Store.Get(owner); ok {
		if _, dup := sc.DBs[req.DBAlias]; dup {
			writeError(w, &apiError{409, "ALREADY_INITIALIZED", fmt.Sprintf("schema %s is already on branch %s (folder %s/)", sc.Name, branch, sc.Dir), nil})
			return
		}
	}
	if !s.initializing.acquire(owner) {
		writeError(w, &apiError{409, "INIT_IN_PROGRESS", fmt.Sprintf("an init of %s is already running", owner), nil})
		return
	}
	who := identityFrom(r.Context())
	id := s.Jobs.Start(s.BaseCtx, "init", func(ctx context.Context, h *jobs.Handle) (any, error) {
		defer s.initializing.release(owner)
		res, err := s.runInit(ctx, h, owner, dir, req.DBAlias, branch, who)
		if err != nil {
			s.Log.Error("init failed", "schema", owner, "db", req.DBAlias, "by", who.Email, "err", err)
			return nil, err
		}
		s.Log.Info("init done", "audit", true, "action", "init", "schema", owner, "db", req.DBAlias,
			"by_name", who.Name, "by_email", who.Email, "objects", res.Objects, "commit", res.Commit)
		return res, nil
	})
	writeJSON(w, http.StatusAccepted, map[string]string{"job_id": id})
}

// pushAttempts bounds how often an init is redone because someone else moved
// the database branch meanwhile (every owner of a database shares it).
const pushAttempts = 3

// runInit adds the owner's folder, built from the objects of database db, to
// the branch of that database (spec §9.1). The owner's first init creates the
// owner baseline, a root commit holding only the folder; every database merges
// that same commit, so the owner's files share an ancestor on all branches.
func (s *Server) runInit(ctx context.Context, h *jobs.Handle, owner, dir, db, branch string, who Identity) (*InitResult, error) {
	exclude := s.Cfg.Exclude
	if len(exclude) == 0 {
		exclude = ci.DefaultExclude
	}

	h.Progress("listing objects of %s from db %s", owner, db)
	objs, err := s.Catalog.Objects(ctx, db, owner, exclude)
	if err != nil {
		return nil, fmt.Errorf("read schema %s from db %s: %w", owner, db, err)
	}
	if len(objs) == 0 {
		return nil, fmt.Errorf("schema %s has no manageable objects in db %s (does it exist, and can the read-only account see it?)", owner, db)
	}
	h.Progress("building %d stub files", len(objs))
	shell := export.BuildShell(objs)
	stubs := make(map[string][]byte, len(shell.Files)) // branch path -> stub
	times := make(map[string]time.Time, len(shell.Files))
	for p, c := range shell.Files {
		stubs[dir+"/"+p] = []byte(c)
		times[dir+"/"+p] = shell.DDLTimes[p]
	}

	opts := gitops.CommitOpts{
		Author:    gitops.Identity{Name: who.Name, Email: who.Email},
		Committer: gitops.Identity{Name: s.Cfg.Git.Committer.Name, Email: s.Cfg.Git.Committer.Email},
		Trailers: []gitops.Trailer{{Key: "OVC-User", Value: who.Email},
			{Key: "OVC-Init", Value: owner + " db=" + db}},
	}
	res := &InitResult{Schema: owner, DB: db, Branch: branch, Dir: dir, Objects: len(objs), ByType: map[string]int{},
		Warnings: shell.Warnings}
	for _, o := range objs {
		res.ByType[string(o.Type)]++
	}

	h.Progress("refreshing the mirror of the remote")
	if err := s.Git.Fetch(ctx); err != nil {
		return nil, fmt.Errorf("fetch remote: %w", err)
	}
	existing, registered := s.Store.Get(owner)
	var baseFiles []string
	if registered {
		res.Baseline = existing.Baseline
		if baseFiles, err = s.Git.Files(ctx, existing.Baseline); err != nil {
			return nil, fmt.Errorf("owner baseline %s of %s is not on the remote any more: %w", existing.Baseline, owner, err)
		}
	} else {
		files := make(map[string][]byte, len(stubs)+1)
		for p, c := range stubs {
			files[p] = c
		}
		y, err := ci.OvcYAML(owner, exclude, s.Cfg.Envs)
		if err != nil {
			return nil, err
		}
		files[dir+"/ovc.yaml"] = y
		o := opts
		o.Message = fmt.Sprintf("ovc init %s --db %s: baseline of %s (%d objects)", owner, db, owner, len(objs))
		if res.Baseline, err = s.Git.RootCommit(ctx, files, o); err != nil {
			return nil, err
		}
		res.FirstDB, res.FilesCount = true, len(files)
	}
	if registered {
		changes := alignChanges(dir, baseFiles, stubs)
		for _, c := range changes {
			if c.Delete {
				res.Removed++
			} else {
				res.Added++
			}
		}
		res.FilesCount = len(baseFiles) + res.Added - res.Removed
	}
	ciFiles, err := ci.Files(s.ciSettings, s.Cfg.Envs)
	if err != nil {
		return nil, err
	}

	unlock, err := s.Git.Lock(ctx, branch)
	if err != nil {
		return nil, err
	}
	defer unlock()
	var prev string // branch head before this init ("" = branch is new)
	for attempt := 1; ; attempt++ {
		if attempt > 1 {
			h.Progress("branch %s moved on the remote, redoing (attempt %d)", branch, attempt)
			if err := s.Git.Fetch(ctx); err != nil {
				return nil, fmt.Errorf("fetch remote: %w", err)
			}
		}
		prev, _ = s.Git.Head(ctx, branch)
		res.NewBranch = prev == ""
		head, err := s.writeInit(ctx, h, res, baseFiles, stubs, ciFiles, prev, opts)
		if err != nil {
			s.undoInit(branch, prev, head, false)
			return nil, err
		}
		h.Progress("pushing %s", branch)
		if s.beforePush != nil {
			s.beforePush(branch)
		}
		err = s.Git.Push(ctx, branch)
		if err == nil {
			res.Commit = head
			break
		}
		s.undoInit(branch, prev, head, false)
		if !errors.Is(err, gitops.ErrRejected) || attempt == pushAttempts {
			return nil, fmt.Errorf("push %s (rolled back): %w", branch, err)
		}
	}

	if err := s.Store.SaveDDLTimes(owner, db, times); err != nil {
		s.undoInit(branch, prev, res.Commit, true)
		return nil, fmt.Errorf("save last_ddl_time (rolled back): %w", err)
	}
	now := time.Now().UTC()
	d := store.DB{Branch: branch, Commit: res.Commit, Objects: len(objs), CreatedBy: who.Email, CreatedAt: now}
	if !registered {
		err = s.Store.Add(store.Schema{Name: owner, Dir: dir, Baseline: res.Baseline, DBs: map[string]store.DB{db: d},
			CreatedBy: who.Email, CreatedAt: now})
	} else {
		err = s.Store.AddDB(owner, db, d)
	}
	if err != nil {
		s.undoInit(branch, prev, res.Commit, true)
		if errors.Is(err, store.ErrExists) || errors.Is(err, store.ErrDBExists) {
			return nil, fmt.Errorf("%s on db %s was registered meanwhile (rolled back)", owner, db)
		}
		return nil, fmt.Errorf("save registry (rolled back): %w", err)
	}
	return res, nil
}

// writeInit puts the owner's folder on the branch (locally) and returns the
// new head: create the branch at the owner baseline or merge the baseline's
// folder into it, then align the folder with this database when the baseline
// came from another one.
func (s *Server) writeInit(ctx context.Context, h *jobs.Handle, res *InitResult, baseFiles []string, stubs, ciFiles map[string][]byte, prev string, opts gitops.CommitOpts) (string, error) {
	b, dir := res.Branch, res.Dir
	var head string
	var err error
	if prev == "" {
		h.Progress("creating branch %s", b)
		if head, err = s.Git.BranchFrom(ctx, b, res.Baseline); err != nil {
			return "", fmt.Errorf("create branch %s: %w", b, err)
		}
		if len(ciFiles) > 0 {
			changes := make([]gitops.Change, 0, len(ciFiles))
			for p, c := range ciFiles {
				changes = append(changes, gitops.Change{Path: p, Content: c})
			}
			sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
			o := opts
			o.Message = fmt.Sprintf("ovc: CI files of database branch %s", b)
			if head, err = s.Git.Commit(ctx, b, head, changes, o); err != nil {
				return head, fmt.Errorf("commit CI files: %w", err)
			}
		}
	} else {
		h.Progress("adding %s/ to branch %s", dir, b)
		o := opts
		o.Message = fmt.Sprintf("ovc init %s --db %s: add %s/ (%d objects)", res.Schema, res.DB, dir, res.Objects)
		if head, err = s.Git.MergeDir(ctx, b, prev, res.Baseline, dir, o); err != nil {
			if errors.Is(err, gitops.ErrExists) {
				return "", fmt.Errorf("branch %s already has a folder %s/ that OVC did not register; refusing to overwrite", b, dir)
			}
			return "", fmt.Errorf("merge %s/ into %s: %w", dir, b, err)
		}
	}
	if res.FirstDB {
		return head, nil
	}
	changes := alignChanges(dir, baseFiles, stubs)
	if len(changes) == 0 {
		return head, nil
	}
	h.Progress("aligning %s/ with db %s (+%d -%d stubs)", dir, res.DB, res.Added, res.Removed)
	o := opts
	o.Message = fmt.Sprintf("ovc init %s --db %s: align %s/ with db %s (+%d -%d objects)", res.Schema, res.DB, dir, res.DB, res.Added, res.Removed)
	if head, err = s.Git.Commit(ctx, b, head, changes, o); err != nil {
		return head, fmt.Errorf("commit %s: %w", b, err)
	}
	return head, nil
}

// alignChanges turns the owner baseline's folder into this database's stubs:
// add stubs only this database has, delete object files it lacks. ovc.yaml,
// migrations and grants are left alone.
func alignChanges(dir string, baseFiles []string, stubs map[string][]byte) []gitops.Change {
	inBase := make(map[string]bool, len(baseFiles))
	var changes []gitops.Change
	for _, p := range baseFiles {
		inBase[p] = true
		rel, ok := strings.CutPrefix(p, dir+"/")
		if _, keep := stubs[p]; ok && !keep && isObjectFile(rel) {
			changes = append(changes, gitops.Change{Path: p, Delete: true})
		}
	}
	for p, c := range stubs {
		if !inBase[p] {
			changes = append(changes, gitops.Change{Path: p, Content: c})
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes
}

// undoInit puts branch back where it was before the init: a branch the init
// created is deleted, an existing one is rewound to prev. pushed says whether
// head reached the remote.
func (s *Server) undoInit(branch, prev, head string, pushed bool) {
	ctx := context.Background()
	if prev == "" {
		_ = s.Git.DeleteBranch(ctx, branch, pushed)
		return
	}
	if head != "" && head != prev {
		if err := s.Git.Rewind(ctx, branch, head, prev, pushed); err != nil {
			s.Log.Error("rollback of init failed", "branch", branch, "from", head, "to", prev, "err", err)
		}
	}
}

// isObjectFile reports whether p (relative to an owner folder) stands for one
// database object, as opposed to ovc.yaml, migrations or grants.
func isObjectFile(p string) bool {
	e, err := layout.Parse(p)
	return err == nil && e.Type != layout.Migration && e.Type != layout.Grants
}
