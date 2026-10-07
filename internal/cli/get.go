package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"ovc/internal/client"
	"ovc/internal/gitlocal"
	"ovc/internal/layout"
)

func newGetCmd() *cobra.Command {
	var db, remote string
	var types []string
	var noMerge bool
	cmd := &cobra.Command{
		Use:   "get <file|folder|[OWNER.]NAME>...",
		Short: "Give objects their real content on the remote and bring it into your branch",
		Long: "Run inside a git clone of the repository. For every object asked for:\n" +
			"  - still a stub on the remote: OVC Server reads its DDL from the database and\n" +
			"    commits it on the database branch;\n" +
			"  - already has content on the remote: nothing to do on the server.\n" +
			"Then the database branch is fetched and brought into your current branch\n" +
			"(fast-forward, or a merge like git pull), so the content arrives as the very\n" +
			"commit that is on the remote and your merge request only shows your edits.\n\n" +
			"Examples:\n" +
			"  ovc get HR/packages/PKG_EMPLOYEE.pkb\n" +
			"  ovc get HR.PKG_EMPLOYEE          (package spec and body)\n" +
			"  ovc get P_SYNC_EMPLOYEE          (searched in every owner folder)\n" +
			"  ovc get HR/procedures/           (every procedure of HR)\n" +
			"  ovc get HR --type view           (every view of HR)",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := newClient(cmd.Context())
			if err != nil {
				return err
			}
			branch, err := layout.DBBranch(db)
			if err != nil {
				return coded(ExitUsage, "--db: %v", err)
			}
			typeSet, err := parseTypes(types)
			if err != nil {
				return coded(ExitUsage, "%v", err)
			}
			ctx, stop := signalContext(cmd)
			defer stop()
			return runGet(ctx, cmd.OutOrStdout(), c, getOptions{db: db, branch: branch, remote: remote, types: typeSet, merge: !noMerge}, args)
		},
	}
	cmd.Flags().StringVar(&db, "db", "dev", "database alias = branch to hydrate on and bring in (prd for a hotfix)")
	cmd.Flags().StringVar(&remote, "remote", "origin", "git remote of the repository")
	cmd.Flags().StringSliceVar(&types, "type", nil, "only these object types, e.g. procedure,package,view")
	cmd.Flags().BoolVar(&noMerge, "no-merge", false, "only hydrate and fetch; do not touch the current branch")
	return cmd
}

type getOptions struct {
	db, branch, remote string
	types              map[layout.ObjectType]bool
	merge              bool
}

// parseTypes maps CLI type names to object types; "package" and "type" mean
// both spec and body.
func parseTypes(names []string) (map[layout.ObjectType]bool, error) {
	out := map[layout.ObjectType]bool{}
	for _, n := range names {
		t := layout.ObjectType(strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(n), "_", " ")))
		switch t {
		case layout.PackageSpec:
			out[layout.PackageSpec], out[layout.PackageBody] = true, true
		case layout.TypeSpec:
			out[layout.TypeSpec], out[layout.TypeBody] = true, true
		default:
			if layout.Dir(t) == "" || t == layout.Migration || t == layout.Grants {
				return nil, fmt.Errorf("unknown object type %q", n)
			}
			out[t] = true
		}
	}
	return out, nil
}

// objectFile is a branch path that stands for one object of an owner.
type objectFile struct {
	path  string // on the branch, e.g. HR/packages/P.pkb
	owner string
	rel   string // inside the owner folder
	entry layout.Entry
}

func runGet(ctx context.Context, out io.Writer, c *client.Client, o getOptions, args []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	repo, err := gitlocal.Open(ctx, cwd)
	if err != nil {
		return coded(ExitUsage, "%v", err)
	}
	repo.Out = out
	ref := gitlocal.RemoteRef(o.remote, o.branch)
	fmt.Fprintf(out, "fetching %s/%s\n", o.remote, o.branch)
	if err := repo.Fetch(ctx, o.remote, o.branch); err != nil {
		return coded(ExitDenied, "git fetch %s %s: %v", o.remote, o.branch, err)
	}
	tree, err := repo.Tree(ctx, ref)
	if err != nil {
		return err
	}
	var objects []objectFile
	for _, e := range tree {
		owner, rel, ok := layout.SplitOwner(e.Path)
		if !ok {
			continue
		}
		ent, err := layout.Parse(rel)
		if err != nil || ent.Type == layout.Migration || ent.Type == layout.Grants {
			continue
		}
		objects = append(objects, objectFile{path: e.Path, owner: owner, rel: rel, entry: ent})
	}

	// Compare real paths: on macOS /var is /private/var, and git prints the
	// resolved top level while os.Getwd may not.
	root := realPath(repo.Root)
	targets, err := resolveTargets(root, realPath(cwd), objects, args, o.types)
	if err != nil {
		return coded(ExitUsage, "%v", err)
	}

	// Every target goes to the server: stubs are hydrated, and objects that
	// have content are checked against the database (spec §9.8 step 3).
	byOwner := map[string][]string{}
	for _, t := range targets {
		byOwner[t.owner] = append(byOwner[t.owner], t.rel)
	}
	fmt.Fprintf(out, "%d object file(s) in %d owner(s)\n", len(targets), len(byOwner))

	failed, written := 0, 0
	var conflicts []string // branch paths
	for _, owner := range sortedOwners(byOwner) {
		res, err := hydrateOwner(ctx, out, c, owner, o.db, byOwner[owner])
		if err != nil {
			return err
		}
		written += len(res.Hydrated) + len(res.Synced)
		failed += len(res.Failed)
		for _, cf := range res.Conflicts {
			conflicts = append(conflicts, owner+"/"+cf.Path)
		}
	}

	if written > 0 {
		if err := repo.Fetch(ctx, o.remote, o.branch); err != nil {
			return coded(ExitDenied, "git fetch %s %s: %v", o.remote, o.branch, err)
		}
	}
	if !o.merge {
		fmt.Fprintf(out, "fetched %s/%s; bring it into your branch with: git merge %s/%s\n", o.remote, o.branch, o.remote, o.branch)
		return resultErr(failed, conflicts)
	}
	cur := repo.CurrentBranch(ctx)
	if cur == "" {
		fmt.Fprintf(out, "detached HEAD: not merging; run git merge %s/%s on a branch\n", o.remote, o.branch)
		return resultErr(failed, conflicts)
	}
	outcome, err := repo.Integrate(ctx, ref)
	if err != nil {
		return coded(ExitConflict, "could not bring %s/%s into %s (the content is on the remote):\n%v\n"+
			"commit or stash your changes, or resolve the merge with git, then: git merge %s/%s",
			o.remote, o.branch, cur, err, o.remote, o.branch)
	}
	switch outcome {
	case gitlocal.UpToDate:
		fmt.Fprintf(out, "%s already contains %s/%s\n", cur, o.remote, o.branch)
	case gitlocal.FastForward:
		fmt.Fprintf(out, "%s fast-forwarded to %s/%s\n", cur, o.remote, o.branch)
	case gitlocal.Merged:
		fmt.Fprintf(out, "merged %s/%s into %s\n", o.remote, o.branch, cur)
	}

	// Report what is now usable in the working copy.
	var stillStub []string
	for _, t := range targets {
		b, err := os.ReadFile(filepath.Join(repo.Root, filepath.FromSlash(t.path)))
		if err != nil || layout.IsStub(b) {
			stillStub = append(stillStub, t.path)
		}
	}
	fmt.Fprintf(out, "\n%d of %d file(s) have their content locally\n", len(targets)-len(stillStub), len(targets))
	for _, p := range stillStub {
		fmt.Fprintf(out, "  still a stub: %s\n", p)
	}
	if len(stillStub) > 0 && failed == 0 {
		failed = len(stillStub)
	}
	return resultErr(failed, conflicts)
}

// resultErr: conflicts with the database come first (exit 5), then objects
// that could not be fetched (exit 1).
func resultErr(failed int, conflicts []string) error {
	if len(conflicts) > 0 {
		return coded(ExitConflict, "%d object(s) changed both on the database and on the branch: you have the branch version; "+
			"wait for their sync merge request (above) before editing them: %s", len(conflicts), strings.Join(conflicts, ", "))
	}
	if failed > 0 {
		return coded(ExitError, "%d object(s) could not be fetched (see above)", failed)
	}
	return nil
}

func sortedOwners(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func hydrateOwner(ctx context.Context, out io.Writer, c *client.Client, owner, db string, rels []string) (*client.HydrateResult, error) {
	sort.Strings(rels)
	fmt.Fprintf(out, "checking %d object file(s) of %s against db %s\n", len(rels), owner, db)
	id, err := c.StartHydrate(ctx, owner, client.HydrateRequest{DB: db, Paths: rels})
	if err != nil {
		return nil, mapAPIError(err)
	}
	j, err := c.WaitJob(ctx, id, 500*time.Millisecond, func(p string) { fmt.Fprintf(out, "  %s\n", p) })
	if err != nil {
		return nil, err
	}
	if j.Status != "succeeded" {
		return nil, coded(ExitError, "hydrate of %s failed: %s", owner, j.Error)
	}
	var res client.HydrateResult
	if err := json.Unmarshal(j.Result, &res); err != nil {
		return nil, err
	}
	list := func(title string, ps []string) {
		if len(ps) == 0 {
			return
		}
		fmt.Fprintf(out, "  %s: %d\n", title, len(ps))
		for i, p := range ps {
			if i == 20 {
				fmt.Fprintf(out, "    ... and %d more\n", len(ps)-20)
				break
			}
			fmt.Fprintf(out, "    %s/%s\n", owner, p)
		}
	}
	if n := len(res.Hydrated) + len(res.Synced); n > 0 {
		fmt.Fprintf(out, "  committed %d file(s) on %s: %s\n", n, res.Branch, shortSHA(res.Commit))
	}
	list("hydrated (stub -> content from the database)", res.Hydrated)
	list("synced (edited on the database outside OVC, now on "+res.Branch+")", res.Synced)
	if len(res.UpToDate) > 0 {
		fmt.Fprintf(out, "  up to date with the database: %d\n", len(res.UpToDate))
	}
	list("changed on "+res.Branch+", not deployed yet (database untouched)", res.Pending)
	for _, cf := range res.Conflicts {
		fmt.Fprintf(out, "  CONFLICT %s/%s: %s\n", owner, cf.Path, cf.Reason)
		state := "opened"
		if cf.Existing {
			state = "still open"
		}
		if cf.Branch != "" {
			fmt.Fprintf(out, "    sync branch (%s): %s\n", state, cf.Branch)
		}
		if cf.URL != "" {
			fmt.Fprintf(out, "    merge request: %s\n", cf.URL)
		}
		if len(cf.Authors) > 0 {
			fmt.Fprintf(out, "    to be resolved by: %s\n", strings.Join(cf.Authors, ", "))
		}
		if cf.Error != "" {
			fmt.Fprintf(out, "    note: %s\n", cf.Error)
		}
	}
	for _, s := range res.Skipped {
		fmt.Fprintf(out, "  skipped %s/%s: %s\n", owner, s.Path, s.Reason)
	}
	for _, f := range res.Failed {
		fmt.Fprintf(out, "  FAILED  %s/%s: %s\n", owner, f.Path, f.Reason)
	}
	for _, w := range res.Warnings {
		fmt.Fprintf(out, "  warning: %s\n", w)
	}
	return &res, nil
}

func shortSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// resolveTargets turns the arguments into object files of the remote branch:
//
//	a file path or a folder path (relative to cwd), or
//	OWNER.NAME / NAME of an object (every file of that name: spec and body).
func resolveTargets(root, cwd string, objects []objectFile, args []string, types map[layout.ObjectType]bool) ([]objectFile, error) {
	want := map[string]objectFile{}
	keep := func(f objectFile) bool { return len(types) == 0 || types[f.entry.Type] }
	for _, arg := range args {
		matched := 0
		if rel, ok := repoPath(root, cwd, arg); ok {
			for _, f := range objects {
				if (f.path == rel || rel == "" || strings.HasPrefix(f.path, rel+"/")) && keep(f) {
					want[f.path] = f
					matched++
				}
			}
			if matched > 0 {
				continue
			}
			if pathExists(objects, rel) {
				return nil, fmt.Errorf("%s has no object of the requested types", arg)
			}
			if strings.ContainsAny(arg, `/\`) {
				return nil, fmt.Errorf("%s: no such object file or folder on the branch", arg)
			}
			// not a path: try it as an object name below
		}
		owner, name := "", arg
		if i := strings.Index(arg, "."); i > 0 && !strings.ContainsAny(arg, `/\`) {
			owner, name = strings.ToUpper(arg[:i]), arg[i+1:]
		}
		var hits []objectFile
		for _, exact := range []bool{true, false} {
			for _, f := range objects {
				if owner != "" && f.owner != owner {
					continue
				}
				if (exact && f.entry.Name == name) || (!exact && strings.EqualFold(f.entry.Name, name)) {
					hits = append(hits, f)
				}
			}
			if len(hits) > 0 {
				break
			}
		}
		owners := map[string]bool{}
		for _, f := range hits {
			owners[f.owner] = true
		}
		if len(owners) > 1 {
			list := make([]string, 0, len(owners))
			for o := range owners {
				list = append(list, o+"."+name)
			}
			sort.Strings(list)
			return nil, fmt.Errorf("%s exists in several owners; say which: %s", arg, strings.Join(list, ", "))
		}
		for _, f := range hits {
			if keep(f) {
				want[f.path] = f
				matched++
			}
		}
		if matched == 0 {
			return nil, fmt.Errorf("%s: no object file, folder or object of that name on the branch", arg)
		}
	}
	out := make([]objectFile, 0, len(want))
	for _, f := range want {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	if len(out) == 0 {
		return nil, errors.New("nothing to get")
	}
	return out, nil
}

// repoPath turns an argument into a path relative to the repository root
// when it is a path inside it ("" = the root itself).
func repoPath(root, cwd, arg string) (string, bool) {
	p := arg
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	} else {
		p = realPath(p)
	}
	r, err := filepath.Rel(root, p)
	if err != nil {
		return "", false
	}
	r = filepath.ToSlash(r)
	if r == "." {
		return "", true
	}
	if r == ".." || strings.HasPrefix(r, "../") {
		return "", false
	}
	return path.Clean(r), true
}

func realPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

func pathExists(objects []objectFile, rel string) bool {
	for _, f := range objects {
		if f.path == rel || strings.HasPrefix(f.path, rel+"/") {
			return true
		}
	}
	return false
}
