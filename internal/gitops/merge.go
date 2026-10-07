package gitops

import (
	"context"
	"fmt"
	"regexp"
	"strings"
)

// ConflictKind classifies a conflicted path (spec §9.3).
type ConflictKind string

const (
	ConflictContent      ConflictKind = "content"       // both sides changed overlapping lines
	ConflictModifyDelete ConflictKind = "modify/delete" // one side deleted, the other changed
	ConflictAddAdd       ConflictKind = "add/add"       // both sides added different content
)

type Conflict struct {
	Path string
	Kind ConflictKind
}

// MergeResult is the outcome of a three-way merge.
type MergeResult struct {
	Tree      string     // merged tree (contains conflict markers for conflicted files)
	Clean     bool       // no conflicts
	Conflicts []Conflict // sorted by path
	// Updates are the files whose merged content differs from "ours" (the
	// caller's local state): what the CLI must write back after a pull.
	// Conflicted files appear here with markers "<<<<<<< local" / "=======" /
	// ">>>>>>> remote". Delete=true means the file must be removed locally.
	Updates []Change
}

// Merge performs the three-way merge of spec §9.3:
//
//	base   = the tree at baseSHA
//	ours   = base with the caller's local changes applied
//	theirs = the commit theirSHA (head of the branch)
//
// It never moves a branch.
func (r *Repo) Merge(ctx context.Context, baseSHA, theirSHA string, local []Change) (*MergeResult, error) {
	id := Identity{Name: "ovc", Email: "ovc@localhost"}
	oursSHA, err := r.makeCommit(ctx, baseSHA, "", local, CommitOpts{Author: id, Committer: id, Message: "ovc local state"})
	if err != nil {
		return nil, err
	}
	out, err := r.git(ctx, nil, nil, "merge-tree", "--write-tree", "--name-only", "--no-messages", "-z",
		"--merge-base="+baseSHA, oursSHA, theirSHA)
	exit1 := false
	if err != nil {
		// exit status 1 means "conflicts"; the tree and conflict list are on stdout.
		if ee, ok := asExit(err); ok && ee == 1 && len(out) > 0 {
			exit1 = true
		} else {
			return nil, err
		}
	}
	parts := strings.Split(string(out), "\x00")
	res := &MergeResult{Tree: parts[0], Clean: !exit1}
	for _, p := range parts[1:] {
		if p == "" {
			break
		}
		res.Conflicts = append(res.Conflicts, Conflict{Path: p})
	}
	for i := range res.Conflicts {
		k, err := r.conflictKind(ctx, baseSHA, oursSHA, theirSHA, res.Conflicts[i].Path)
		if err != nil {
			return nil, err
		}
		res.Conflicts[i].Kind = k
	}
	res.Updates, err = r.updates(ctx, oursSHA, res.Tree, theirSHA, res.Conflicts)
	return res, err
}

func asExit(err error) (int, bool) {
	var ge *gitError
	if !errorsAs(err, &ge) {
		return 0, false
	}
	type exitCoder interface{ ExitCode() int }
	if ec, ok := ge.err.(exitCoder); ok {
		return ec.ExitCode(), true
	}
	return 0, false
}

func (r *Repo) exists(ctx context.Context, ref, p string) bool {
	_, err := r.git(ctx, nil, nil, "cat-file", "-e", ref+":"+p)
	return err == nil
}

func (r *Repo) conflictKind(ctx context.Context, base, ours, theirs, p string) (ConflictKind, error) {
	inBase, inOurs, inTheirs := r.exists(ctx, base, p), r.exists(ctx, ours, p), r.exists(ctx, theirs, p)
	switch {
	case !inBase && inOurs && inTheirs:
		return ConflictAddAdd, nil
	case inBase && inOurs != inTheirs:
		return ConflictModifyDelete, nil
	}
	return ConflictContent, nil
}

// updates lists what differs between ours and the merged tree, with marker
// labels normalised (git labels sides with commit ids).
func (r *Repo) updates(ctx context.Context, oursSHA, tree, theirsSHA string, conflicts []Conflict) ([]Change, error) {
	out, err := r.git(ctx, nil, nil, "diff-tree", "-r", "--name-status", "-z", "--no-renames", oursSHA, tree)
	if err != nil {
		return nil, err
	}
	f := strings.Split(string(out), "\x00")
	var ups []Change
	for i := 0; i+1 < len(f); i += 2 {
		status, p := f[i], f[i+1]
		if status == "D" {
			ups = append(ups, Change{Path: p, Delete: true})
			continue
		}
		blob, err := r.git(ctx, nil, nil, "cat-file", "blob", tree+":"+p)
		if err != nil {
			return nil, err
		}
		ups = append(ups, Change{Path: p, Content: blob})
	}
	conflicted := map[string]bool{}
	for _, c := range conflicts {
		conflicted[c.Path] = true
	}
	for i := range ups {
		if conflicted[ups[i].Path] && !ups[i].Delete {
			ups[i].Content = relabel(ups[i].Content, oursSHA, theirsSHA)
		}
	}
	// A conflicted file that merge-tree left identical to ours (e.g. we
	// modified, they deleted: the modified file stays) is not in diff-tree.
	// Callers still learn about it from Conflicts.
	return ups, nil
}

var markerRe = regexp.MustCompile(`(?m)^(<<<<<<<|>>>>>>>) ([0-9a-f]{40})$`)

func relabel(content []byte, oursSHA, theirsSHA string) []byte {
	return markerRe.ReplaceAllFunc(content, func(m []byte) []byte {
		parts := strings.SplitN(string(m), " ", 2)
		switch parts[1] {
		case oursSHA:
			return []byte(parts[0] + " local")
		case theirsSHA:
			return []byte(parts[0] + " remote")
		}
		return m
	})
}

// HasConflictMarkers reports whether content still contains the markers
// written by Merge (push refuses such files; spec §9.3, §9.4).
func HasConflictMarkers(content []byte) bool {
	s := string(content)
	return strings.Contains(s, "\n<<<<<<< local") || strings.HasPrefix(s, "<<<<<<< local") ||
		strings.Contains(s, "\n>>>>>>> remote") || strings.HasPrefix(s, ">>>>>>> remote")
}

func (c Conflict) String() string { return fmt.Sprintf("%s (%s)", c.Path, c.Kind) }
