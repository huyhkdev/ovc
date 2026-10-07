package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"ovc/internal/ci"
	"ovc/internal/gitops"
	"ovc/internal/jobs"
	"ovc/internal/layout"
	"ovc/internal/oracle/export"
	"ovc/internal/store"
)

// stubMaxSize: files larger than this cannot be stubs, so they are not read
// when looking for stubs (a stub line is about 40 bytes).
const stubMaxSize = 256

// lookupSchemaDB resolves {s} and the db of a request to a registered owner
// that has been initialised on that database.
func (s *Server) lookupSchemaDB(r *http.Request, db string) (store.Schema, string, *apiError) {
	name := strings.ToUpper(chi.URLParam(r, "schema"))
	sc, ok := s.Store.Get(name)
	if !ok {
		return sc, "", &apiError{404, "SCHEMA_NOT_FOUND", fmt.Sprintf("schema %s is not registered (ovc init %s --db <alias>)", name, name), nil}
	}
	if db == "" {
		db = "dev"
	}
	d, ok := sc.DBs[db]
	if !ok {
		have := make([]string, 0, len(sc.DBs))
		for k := range sc.DBs {
			have = append(have, k)
		}
		sort.Strings(have)
		return sc, "", &apiError{404, "DB_NOT_INITIALIZED", fmt.Sprintf("schema %s is not on db %s (have %v)", sc.Name, db, have), nil}
	}
	return sc, d.Branch, nil
}

type hydrateRequest struct {
	DB    string   `json:"db"`
	Paths []string `json:"paths"` // relative to the owner's folder
	All   bool     `json:"all"`   // every object file of the owner (optionally limited by Types)
	Types []string `json:"types"` // object types, e.g. "PROCEDURE", "PACKAGE BODY"
}

// HydrateResult is the job result of `ovc get` on the server (spec §9.8).
// Paths are relative to the owner's folder.
type HydrateResult struct {
	Schema    string         `json:"schema"`
	DB        string         `json:"db"`
	Branch    string         `json:"branch"`
	Commit    string         `json:"commit"`     // branch head afterwards
	Hydrated  []string       `json:"hydrated"`   // stub -> DDL of the database
	Synced    []string       `json:"synced"`     // edited by hand on the database -> branch updated
	UpToDate  []string       `json:"up_to_date"` // branch and database agree
	Pending   []string       `json:"pending"`    // branch has changes not deployed yet; database untouched
	Conflicts []SyncConflict `json:"conflicts"`  // changed on both sides: a sync branch and MR
	Skipped   []PathReason   `json:"skipped,omitempty"`
	Failed    []PathReason   `json:"failed,omitempty"`
	Warnings  []string       `json:"warnings,omitempty"`
	ByType    map[string]int `json:"by_type"`
}

// SyncConflict is an object changed both on the database (by hand) and on
// the branch (not deployed yet). OVC does not pick a side (spec §9.8 step 5).
type SyncConflict struct {
	Path     string   `json:"path"`
	Reason   string   `json:"reason"`
	Branch   string   `json:"branch"`            // sync/<db>/... holding the database version
	URL      string   `json:"url,omitempty"`     // its pull/merge request
	Existing bool     `json:"existing"`          // the sync branch was already there
	Authors  []string `json:"authors,omitempty"` // of the undeployed commits: who should resolve
	Error    string   `json:"error,omitempty"`   // e.g. the MR could not be opened
}

type PathReason struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// POST /api/v1/schemas/{schema}/hydrate
func (s *Server) handleHydrate(w http.ResponseWriter, r *http.Request) {
	var req hydrateRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, &apiError{400, "INVALID_REQUEST", "body must be JSON {db, paths[], all, types[]}: " + err.Error(), nil})
		return
	}
	if len(req.Paths) == 0 && !req.All {
		writeError(w, &apiError{400, "INVALID_REQUEST", "give paths or all=true", nil})
		return
	}
	for _, p := range req.Paths {
		if err := gitops.ValidatePath(p); err != nil {
			writeError(w, &apiError{400, "INVALID_PATH", err.Error(), nil})
			return
		}
	}
	types := map[layout.ObjectType]bool{}
	for _, t := range req.Types {
		ot := layout.ObjectType(strings.ToUpper(strings.ReplaceAll(t, "_", " ")))
		if layout.Dir(ot) == "" {
			writeError(w, &apiError{400, "INVALID_TYPE", fmt.Sprintf("unknown object type %q", t), nil})
			return
		}
		types[ot] = true
	}
	if req.DB == "" {
		req.DB = "dev"
	}
	sc, branch, aerr := s.lookupSchemaDB(r, req.DB)
	if aerr != nil {
		writeError(w, aerr)
		return
	}
	who := identityFrom(r.Context())
	id := s.Jobs.Start(s.BaseCtx, "hydrate", func(ctx context.Context, h *jobs.Handle) (any, error) {
		res, err := s.runHydrate(ctx, h, sc, req, types, branch, who)
		if err != nil {
			s.Log.Error("hydrate failed", "schema", sc.Name, "db", req.DB, "by", who.Email, "err", err)
			return nil, err
		}
		s.Log.Info("hydrate done", "audit", true, "action", "hydrate", "schema", sc.Name, "db", req.DB,
			"by_name", who.Name, "by_email", who.Email, "objects", len(res.Hydrated), "commit", res.Commit)
		return res, nil
	})
	writeJSON(w, http.StatusAccepted, map[string]string{"job_id": id})
}

// runHydrate makes the branch agree with the database for the requested
// objects, then the CLI brings the branch in (spec §9.8): stubs are hydrated,
// hand edits on the database are synced, and objects changed on both sides
// get a sync branch and a pull/merge request instead of a guess.
func (s *Server) runHydrate(ctx context.Context, h *jobs.Handle, sc store.Schema, req hydrateRequest, types map[layout.ObjectType]bool, branch string, who Identity) (*HydrateResult, error) {
	res := &HydrateResult{Schema: sc.Name, DB: req.DB, Branch: branch, ByType: map[string]int{},
		Hydrated: []string{}, Synced: []string{}, UpToDate: []string{}, Pending: []string{}, Conflicts: []SyncConflict{}}
	dir := sc.Dir

	// The branch lock is held only around git work, not while the database is
	// read (that can take minutes for a whole owner).
	unlock, err := s.Git.Lock(ctx, branch)
	if err != nil {
		return nil, err
	}
	h.Progress("refreshing %s", branch)
	err = s.Git.Fetch(ctx, branch)
	head, herr := s.Git.Head(ctx, branch)
	unlock()
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", branch, err)
	}
	if herr != nil {
		return nil, herr
	}
	tree, err := s.Git.Tree(ctx, head, dir)
	if err != nil {
		return nil, err
	}
	inTree := make(map[string]bool, len(tree))
	for _, e := range tree {
		inTree[strings.TrimPrefix(e.Path, dir+"/")] = true
	}
	stubs, err := s.stubsAt(ctx, head, dir)
	if err != nil {
		return nil, err
	}

	exclude := s.Cfg.Exclude
	if len(exclude) == 0 {
		exclude = ci.DefaultExclude
	}
	h.Progress("listing objects of %s from db %s", sc.Name, req.DB)
	objs, err := s.Catalog.Objects(ctx, req.DB, sc.Name, exclude)
	if err != nil {
		return nil, fmt.Errorf("read schema %s from db %s: %w", sc.Name, req.DB, err)
	}
	byPath := make(map[string]export.Object, len(objs))
	for _, o := range objs {
		if p, err := layout.PathFor(o.Type, o.Name); err == nil {
			byPath[p] = o
		}
	}

	// Targets: object files of the owner, relative to its folder.
	want := map[string]bool{}
	pick := func(p string) {
		if e, err := layout.Parse(p); err == nil && e.Type != layout.Migration && e.Type != layout.Grants &&
			(len(types) == 0 || types[e.Type]) {
			want[p] = true
		}
	}
	if req.All {
		for p := range inTree {
			pick(p)
		}
	}
	for _, p := range req.Paths {
		p = path.Clean(p)
		if !inTree[p] {
			res.Failed = append(res.Failed, PathReason{p, "no such file in " + dir + "/ on " + branch})
			continue
		}
		e, err := layout.Parse(p)
		if err != nil || e.Type == layout.Migration || e.Type == layout.Grants {
			res.Skipped = append(res.Skipped, PathReason{p, "not an object file"})
			continue
		}
		want[p] = true
	}
	// A table comes with its indexes and foreign keys (spec §9.8 step 2).
	for p := range want {
		if e, _ := layout.Parse(p); e.Type == layout.Table {
			for _, rel := range export.Related(objs, e.Name) {
				if rp, err := layout.PathFor(rel.Type, rel.Name); err == nil && inTree[rp] {
					want[rp] = true
				}
			}
		}
	}

	times, _ := s.Store.DDLTimes(sc.Name, req.DB) // missing: no warnings, not an error
	var targets []export.Object
	var targetPaths, contentPaths []string
	for _, p := range sortedKeys(want) {
		o, ok := byPath[p]
		if !ok {
			if stubs[p] {
				res.Failed = append(res.Failed, PathReason{p, "object is not in db " + req.DB + " any more (dropped or excluded)"})
			} else {
				res.Warnings = append(res.Warnings, fmt.Sprintf("%s is on %s but not in db %s: not deployed yet, or dropped by hand (see drift)", p, branch, req.DB))
			}
			continue
		}
		if t, ok := times[dir+"/"+p]; ok && stubs[p] && o.LastDDLTime.After(t) {
			res.Warnings = append(res.Warnings, fmt.Sprintf("%s changed on db %s since init (last_ddl_time %s, at init %s): recompiled or edited by hand, please check",
				p, req.DB, o.LastDDLTime.Format(time.RFC3339), t.Format(time.RFC3339)))
		}
		targets = append(targets, o)
		targetPaths = append(targetPaths, p)
		if !stubs[p] {
			contentPaths = append(contentPaths, dir+"/"+p)
		}
	}
	if len(targets) == 0 {
		res.Commit = head
		return res, nil
	}

	workers := s.Cfg.Export.Workers
	if workers <= 0 {
		workers = 8
	}
	h.Progress("reading DDL of %d objects from db %s", len(targets), req.DB)
	files, err := s.Catalog.Hydrate(ctx, req.DB, sc.Name, targets, workers)
	if err != nil {
		return nil, fmt.Errorf("read DDL from db %s: %w", req.DB, err)
	}
	remote, err := s.Git.ReadFiles(ctx, head, contentPaths)
	if err != nil {
		return nil, err
	}
	bases, err := s.Store.Bases(sc.Name, req.DB)
	if err != nil {
		return nil, err
	}

	// Decide per object: base / database / branch (spec §9.8 step 3).
	dbContent := map[string][]byte{}
	remoteHash := map[string]string{} // for synced paths: the branch content we decided on
	var hydrate, sync []string
	newBases := map[string]store.Base{}
	var conflicts []string
	for i, f := range files {
		p := targetPaths[i]
		if f.Err != nil {
			res.Failed = append(res.Failed, PathReason{p, f.Err.Error()})
			continue
		}
		if strings.TrimSpace(f.Content) == "" {
			res.Failed = append(res.Failed, PathReason{p, "database returned no DDL"})
			continue
		}
		d := []byte(f.Content)
		dbContent[p] = d
		if stubs[p] {
			hydrate = append(hydrate, p)
			continue
		}
		dh, rh := hashOf(d), hashOf(remote[dir+"/"+p])
		base, hasBase := bases[dir+"/"+p]
		switch {
		case dh == rh:
			res.UpToDate = append(res.UpToDate, p)
			if !hasBase || base.Hash != dh {
				newBases[dir+"/"+p] = store.Base{Hash: dh, Commit: head, At: time.Now().UTC()}
			}
		case !hasBase:
			conflicts = append(conflicts, p)
		case dh == base.Hash:
			res.Pending = append(res.Pending, p) // remote ahead, database untouched
		case rh == base.Hash:
			sync = append(sync, p)
			remoteHash[p] = rh
		default:
			conflicts = append(conflicts, p)
		}
	}

	// Hydrate + sync: one commit on the database branch.
	if len(hydrate)+len(sync) > 0 {
		if unlock, err = s.Git.Lock(ctx, branch); err != nil {
			return nil, err
		}
		sha, doneH, doneS, err := s.commitHydrateSync(ctx, h, sc, req.DB, branch, hydrate, sync, remoteHash, dbContent, res.Warnings, who)
		unlock()
		if err != nil {
			return nil, err
		}
		head = sha
		res.Hydrated, res.Synced = doneH, doneS
		now := time.Now().UTC()
		for _, p := range append(append([]string{}, doneH...), doneS...) {
			newBases[dir+"/"+p] = store.Base{Hash: hashOf(dbContent[p]), Commit: sha, At: now}
			e, _ := layout.Parse(p)
			res.ByType[string(e.Type)]++
		}
		for _, p := range hydrate {
			if !slices.Contains(doneH, p) {
				res.Skipped = append(res.Skipped, PathReason{p, "hydrated by someone else meanwhile"})
			}
		}
		for _, p := range sync {
			if !slices.Contains(doneS, p) {
				res.Skipped = append(res.Skipped, PathReason{p, "changed on " + branch + " meanwhile; run ovc get again"})
			}
		}
	}
	res.Commit = head
	if len(newBases) > 0 {
		if err := s.Store.SaveBases(sc.Name, req.DB, newBases); err != nil {
			return nil, fmt.Errorf("save bases: %w", err)
		}
	}

	for _, p := range conflicts {
		res.Conflicts = append(res.Conflicts, s.openSyncConflict(ctx, h, sc, req.DB, branch, head, p, dbContent[p], bases, who))
	}
	return res, nil
}

// commitHydrateSync writes the hydrated and synced objects in one commit and
// pushes it, redoing it on a new head when the branch moved. It returns what
// it actually wrote: a stub hydrated meanwhile, or a synced file changed on
// the branch meanwhile, is left out.
func (s *Server) commitHydrateSync(ctx context.Context, h *jobs.Handle, sc store.Schema, db, branch string, hydrate, sync []string,
	remoteHash map[string]string, content map[string][]byte, warnings []string, who Identity) (string, []string, []string, error) {
	dir := sc.Dir
	for attempt := 1; ; attempt++ {
		if attempt > 1 {
			h.Progress("branch %s moved on the remote, redoing (attempt %d)", branch, attempt)
			if err := s.Git.Fetch(ctx, branch); err != nil {
				return "", nil, nil, fmt.Errorf("fetch %s: %w", branch, err)
			}
		}
		head, err := s.Git.Head(ctx, branch)
		if err != nil {
			return "", nil, nil, err
		}
		still, err := s.stubsAt(ctx, head, dir)
		if err != nil {
			return "", nil, nil, err
		}
		var doneH, doneS []string
		var changes []gitops.Change
		for _, p := range hydrate {
			if still[p] {
				doneH = append(doneH, p)
				changes = append(changes, gitops.Change{Path: dir + "/" + p, Content: content[p]})
			}
		}
		if len(sync) > 0 {
			paths := make([]string, len(sync))
			for i, p := range sync {
				paths[i] = dir + "/" + p
			}
			now, err := s.Git.ReadFiles(ctx, head, paths)
			if err != nil {
				return "", nil, nil, err
			}
			for _, p := range sync {
				if hashOf(now[dir+"/"+p]) == remoteHash[p] {
					doneS = append(doneS, p)
					changes = append(changes, gitops.Change{Path: dir + "/" + p, Content: content[p]})
				}
			}
		}
		if len(changes) == 0 {
			return head, nil, nil, nil
		}
		var subject []string
		if len(doneH) > 0 {
			subject = append(subject, fmt.Sprintf("hydrate %d", len(doneH)))
		}
		if len(doneS) > 0 {
			subject = append(subject, fmt.Sprintf("sync %d", len(doneS)))
		}
		msg := fmt.Sprintf("ovc get %s: %s object(s) from db %s\n\n", sc.Name, strings.Join(subject, ", "), db)
		list := func(title string, ps []string) {
			if len(ps) == 0 {
				return
			}
			msg += title + ":\n"
			for i, p := range ps {
				if i == 50 {
					msg += fmt.Sprintf("  ... and %d more\n", len(ps)-50)
					break
				}
				msg += "  " + dir + "/" + p + "\n"
			}
		}
		list("hydrated (stub -> DDL of the database)", doneH)
		list("synced (edited on the database outside OVC)", doneS)
		for _, w := range warnings {
			msg += "\nwarning: " + w
		}
		trailers := []gitops.Trailer{{Key: "OVC-User", Value: who.Email}}
		if len(doneH) > 0 {
			trailers = append(trailers, gitops.Trailer{Key: "OVC-Hydrate", Value: fmt.Sprintf("%s db=%s objects=%d", sc.Name, db, len(doneH))})
		}
		if len(doneS) > 0 {
			trailers = append(trailers, gitops.Trailer{Key: "OVC-Sync", Value: fmt.Sprintf("%s db=%s objects=%d", sc.Name, db, len(doneS))})
		}
		opts := gitops.CommitOpts{
			Author:    gitops.Identity{Name: who.Name, Email: who.Email},
			Committer: gitops.Identity{Name: s.Cfg.Git.Committer.Name, Email: s.Cfg.Git.Committer.Email},
			Message:   msg,
			Trailers:  trailers,
		}
		sha, err := s.Git.Commit(ctx, branch, head, changes, opts)
		if err != nil {
			return "", nil, nil, fmt.Errorf("commit on %s: %w", branch, err)
		}
		h.Progress("pushing %s", branch)
		if s.beforePush != nil {
			s.beforePush(branch)
		}
		err = s.Git.Push(ctx, branch)
		if err == nil {
			return sha, doneH, doneS, nil
		}
		_ = s.Git.Rewind(context.Background(), branch, sha, head, false)
		if !errors.Is(err, gitops.ErrRejected) || attempt == pushAttempts {
			return "", nil, nil, fmt.Errorf("push %s: %w", branch, err)
		}
	}
}

var refUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// syncBranchPrefix is the stable part of the sync branch of one object:
// sync/<db>/<OWNER>/<dir>/<file>-<hash8>-; a timestamp completes it. The hash
// of the exact path keeps it unique although unsafe characters are replaced.
func syncBranchPrefix(db, ownerDir, rel string) string {
	parts := append([]string{ownerDir}, strings.Split(rel, "/")...)
	for i, p := range parts {
		parts[i] = strings.Trim(refUnsafe.ReplaceAllString(p, "_"), ".")
		if parts[i] == "" {
			parts[i] = "_"
		}
	}
	return "sync/" + db + "/" + strings.Join(parts, "/") + "-" + hashOf([]byte(ownerDir + "/" + rel))[:8] + "-"
}

// openSyncConflict records one conflicting object (spec §9.8 step 5): unless
// a sync branch for it is still open, push the database version on a new
// branch made from the object's base and open a pull/merge request.
func (s *Server) openSyncConflict(ctx context.Context, h *jobs.Handle, sc store.Schema, db, branch, head, rel string, dbContent []byte,
	bases map[string]store.Base, who Identity) SyncConflict {
	full := sc.Dir + "/" + rel
	c := SyncConflict{Path: rel}
	base, hasBase := bases[full]
	if hasBase {
		c.Reason = fmt.Sprintf("changed on db %s outside OVC and on %s (not deployed yet) since they last agreed", db, branch)
		c.Authors, _ = s.Git.Authors(ctx, base.Commit, head, full)
	} else {
		c.Reason = fmt.Sprintf("db %s and %s differ and OVC has no record of when they last agreed", db, branch)
	}
	prefix := syncBranchPrefix(db, sc.Dir, rel)
	open, err := s.Git.RemoteBranches(ctx, prefix)
	if err != nil {
		c.Error = "list sync branches: " + err.Error()
		return c
	}
	if len(open) > 0 {
		sort.Strings(open)
		c.Branch, c.Existing = open[len(open)-1], true
		return c
	}
	c.Branch = prefix + time.Now().UTC().Format("20060102T150405")
	from := head
	if hasBase {
		from = base.Commit
	}
	h.Progress("conflict on %s: pushing the database version to %s", full, c.Branch)
	if _, err := s.Git.BranchFrom(ctx, c.Branch, from); err != nil {
		c.Error = err.Error()
		return c
	}
	opts := gitops.CommitOpts{
		Author:    gitops.Identity{Name: who.Name, Email: who.Email},
		Committer: gitops.Identity{Name: s.Cfg.Git.Committer.Name, Email: s.Cfg.Git.Committer.Email},
		Message:   fmt.Sprintf("ovc sync %s: %s as it is on db %s\n\n%s", sc.Name, full, db, c.Reason),
		Trailers: []gitops.Trailer{{Key: "OVC-User", Value: who.Email},
			{Key: "OVC-Sync-Conflict", Value: fmt.Sprintf("%s db=%s path=%s", sc.Name, db, full)}},
	}
	if _, err := s.Git.Commit(ctx, c.Branch, from, []gitops.Change{{Path: full, Content: dbContent}}, opts); err != nil {
		_ = s.Git.DeleteBranch(context.Background(), c.Branch, false)
		c.Error = err.Error()
		return c
	}
	if err := s.Git.Push(ctx, c.Branch); err != nil {
		_ = s.Git.DeleteBranch(context.Background(), c.Branch, false)
		c.Error = "push " + c.Branch + ": " + err.Error()
		return c
	}
	if s.Host == nil {
		c.Error = "no Git host API configured: open the merge request by hand"
		return c
	}
	body := fmt.Sprintf("OVC found `%s` changed on database **%s** outside OVC, while `%s` also has changes to it that are not deployed yet.\n\n"+
		"This branch holds the database version, made from the last point where both agreed, so the conflict shows here. "+
		"Merge it into `%s` keeping the database part, the new code, or both. Until this is merged, changes to this object cannot be merged or deployed.\n\n"+
		"- Found by: %s <%s> (ovc get)\n", full, db, branch, branch, who.Name, who.Email)
	if len(c.Authors) > 0 {
		body += "- Undeployed changes by (please resolve): " + strings.Join(c.Authors, ", ") + "\n"
	}
	url, err := s.Host.CreateChangeRequest(ctx, c.Branch, branch, fmt.Sprintf("OVC sync: %s changed on db %s", full, db), body)
	if err != nil {
		c.Error = err.Error()
		return c
	}
	c.URL = url
	return c
}

func hashOf(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// stubsAt returns the paths (relative to dir) of the stub files under dir.
func (s *Server) stubsAt(ctx context.Context, ref, dir string) (map[string]bool, error) {
	tree, err := s.Git.Tree(ctx, ref, dir)
	if err != nil {
		return nil, err
	}
	var small []string
	for _, e := range tree {
		if e.Size <= stubMaxSize {
			small = append(small, e.Path)
		}
	}
	out := map[string]bool{}
	if len(small) == 0 {
		return out, nil
	}
	blobs, err := s.Git.ReadFiles(ctx, ref, small)
	if err != nil {
		return nil, err
	}
	for p, b := range blobs {
		if layout.IsStub(b) {
			out[strings.TrimPrefix(p, dir+"/")] = true
		}
	}
	return out, nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
