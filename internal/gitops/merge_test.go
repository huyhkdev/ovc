package gitops

import (
	"errors"
	"strings"
	"testing"
)

const five = "line1\nline2\nline3\nline4\nline5\n"

func mergeSetup(t *testing.T) (*Repo, string) {
	r, _ := newMirror(t)
	base, err := r.CreateBranch(ctx, "qlsc_dev", files(
		"packages/P.pkb", five,
		"packages/Q.pkb", "q\n",
		"packages/Z.pkb", "z\n",
		"packages/R.pkb", "r\n"), opts)
	if err != nil {
		t.Fatal(err)
	}
	return r, base
}

func byPath(ups []Change) map[string]Change {
	m := map[string]Change{}
	for _, u := range ups {
		m[u.Path] = u
	}
	return m
}

func TestMergeCases(t *testing.T) {
	r, base := mergeSetup(t)
	// remote (theirs): P line1 changed, Q changed, new file N, R deleted
	theirs, err := r.Commit(ctx, "qlsc_dev", base, []Change{
		{Path: "packages/P.pkb", Content: []byte("LINE1\nline2\nline3\nline4\nline5\n")},
		{Path: "packages/Q.pkb", Content: []byte("q-remote\n")},
		{Path: "packages/N.pkb", Content: []byte("new-remote\n")},
		{Path: "packages/R.pkb", Delete: true},
	}, opts)
	if err != nil {
		t.Fatal(err)
	}
	// local (ours): P line5 changed (no overlap), Q changed (overlap), Z changed (remote untouched), R modified (remote deleted)
	res, err := r.Merge(ctx, base, theirs, []Change{
		{Path: "packages/P.pkb", Content: []byte("line1\nline2\nline3\nline4\nLINE5\n")},
		{Path: "packages/Q.pkb", Content: []byte("q-local\n")},
		{Path: "packages/Z.pkb", Content: []byte("z-local\n")},
		{Path: "packages/R.pkb", Content: []byte("r-local\n")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Clean {
		t.Fatal("must not be clean")
	}
	kinds := map[string]ConflictKind{}
	for _, c := range res.Conflicts {
		kinds[c.Path] = c.Kind
	}
	if len(kinds) != 2 || kinds["packages/Q.pkb"] != ConflictContent || kinds["packages/R.pkb"] != ConflictModifyDelete {
		t.Errorf("conflicts = %v", res.Conflicts)
	}
	ups := byPath(res.Updates)
	// auto-merged: both edits present
	if got := string(ups["packages/P.pkb"].Content); got != "LINE1\nline2\nline3\nline4\nLINE5\n" {
		t.Errorf("P merged = %q", got)
	}
	// remote-only new file arrives
	if got := string(ups["packages/N.pkb"].Content); got != "new-remote\n" {
		t.Errorf("N = %q", got)
	}
	// local-only change is not an update (nothing to write back)
	if _, ok := ups["packages/Z.pkb"]; ok {
		t.Error("Z is local-only and must not be rewritten")
	}
	// conflict markers use the spec's labels
	q := string(ups["packages/Q.pkb"].Content)
	if q != "<<<<<<< local\nq-local\n=======\nq-remote\n>>>>>>> remote\n" {
		t.Errorf("Q markers = %q", q)
	}
	if !HasConflictMarkers(ups["packages/Q.pkb"].Content) {
		t.Error("HasConflictMarkers false for conflicted file")
	}
	if HasConflictMarkers([]byte(five)) {
		t.Error("false positive")
	}
	if strings.Contains(q, strings.Repeat("0", 5)) {
		t.Errorf("commit ids leaked into markers: %q", q)
	}
}

func TestMergeCleanVariants(t *testing.T) {
	r, base := mergeSetup(t)
	// nothing changed on either side
	res, err := r.Merge(ctx, base, base, nil)
	if err != nil || !res.Clean || len(res.Updates) != 0 {
		t.Fatalf("no-op merge: %+v, %v", res, err)
	}
	// remote only
	theirs, _ := r.Commit(ctx, "qlsc_dev", base, []Change{{Path: "packages/Q.pkb", Content: []byte("q2\n")}, {Path: "packages/Z.pkb", Delete: true}}, opts)
	res, err = r.Merge(ctx, base, theirs, nil)
	if err != nil || !res.Clean {
		t.Fatalf("remote-only: %+v, %v", res, err)
	}
	ups := byPath(res.Updates)
	if string(ups["packages/Q.pkb"].Content) != "q2\n" || !ups["packages/Z.pkb"].Delete {
		t.Errorf("remote-only updates = %+v", res.Updates)
	}
	// local only
	res, err = r.Merge(ctx, base, base, []Change{{Path: "packages/Q.pkb", Content: []byte("mine\n")}})
	if err != nil || !res.Clean || len(res.Updates) != 0 {
		t.Fatalf("local-only: %+v, %v", res, err)
	}
	// clean merge result can be committed on the branch (push path)
	res, _ = r.Merge(ctx, base, theirs, []Change{{Path: "packages/P.pkb", Content: []byte("line1\nline2\nline3\nline4\nLINE5\n")}})
	if !res.Clean {
		t.Fatal("expected clean")
	}
	sha, err := r.CommitTree(ctx, "qlsc_dev", theirs, res.Tree, opts)
	if err != nil {
		t.Fatal(err)
	}
	if p := gitOut(t, r.dir, "rev-parse", sha+"^"); p != theirs {
		t.Errorf("parent %s != %s", p, theirs)
	}
	if p, _ := r.ReadFile(ctx, "qlsc_dev", "packages/P.pkb"); !strings.Contains(string(p), "LINE5") {
		t.Errorf("local change missing after merge commit: %q", p)
	}
	if q, _ := r.ReadFile(ctx, "qlsc_dev", "packages/Q.pkb"); string(q) != "q2\n" {
		t.Errorf("remote change missing: %q", q)
	}
	if id := gitOut(t, r.dir, "log", "-1", "--format=%an|%cn", "qlsc_dev"); id != "Nguyen Van A|OVC Bot" {
		t.Errorf("identity on merge commit = %q", id)
	}
	if _, err := r.CommitTree(ctx, "qlsc_dev", base, res.Tree, opts); !errors.Is(err, ErrStale) {
		t.Errorf("want ErrStale, got %v", err)
	}
}

func TestMergeDeleteModifyOtherSideAndAddAdd(t *testing.T) {
	r, base := mergeSetup(t)
	// remote modifies R, local deletes it
	theirs, _ := r.Commit(ctx, "qlsc_dev", base, []Change{
		{Path: "packages/R.pkb", Content: []byte("r-remote\n")},
		{Path: "packages/NEW.pkb", Content: []byte("from-remote\n")},
	}, opts)
	res, err := r.Merge(ctx, base, theirs, []Change{
		{Path: "packages/R.pkb", Delete: true},
		{Path: "packages/NEW.pkb", Content: []byte("from-local\n")},
	})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]ConflictKind{}
	for _, c := range res.Conflicts {
		kinds[c.Path] = c.Kind
	}
	if kinds["packages/R.pkb"] != ConflictModifyDelete || kinds["packages/NEW.pkb"] != ConflictAddAdd {
		t.Errorf("conflicts = %v", res.Conflicts)
	}
	if got := string(byPath(res.Updates)["packages/NEW.pkb"].Content); !strings.Contains(got, "<<<<<<< local") || !strings.Contains(got, ">>>>>>> remote") {
		t.Errorf("add/add markers = %q", got)
	}
}
