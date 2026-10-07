package layout

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// Repo layout (spec §5): every long-lived branch is one database, named after
// its alias ("dev", "prd"), and holds one folder per owner (schema):
//
//	dev/  HR/packages/PKG_EMPLOYEE.pkb
//	      QLSC/tables/...
//	      .github/workflows/ovc-deploy.yml
//
// Paths inside an owner folder are what PathFor and Parse handle; OwnerPath
// and SplitOwner add and remove the folder.

var dbBranchRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*$`)

// DBBranch returns the long-lived branch of a database alias: the alias itself.
func DBBranch(alias string) (string, error) {
	if !dbBranchRe.MatchString(alias) || strings.Contains(alias, "..") || strings.HasSuffix(alias, ".lock") {
		return "", fmt.Errorf("layout: db alias %q cannot be a branch name (want lower case letters, digits, _ . -)", alias)
	}
	return alias, nil
}

// OwnerDir returns the folder of an owner, named like the owner with the same
// case encoding as object files ("HR" -> "HR", "Sales" -> "S~ales~").
func OwnerDir(owner string) (string, error) {
	d, err := EncodeName(owner)
	if err != nil {
		return "", fmt.Errorf("layout: owner %q cannot be a folder name: %w", owner, err)
	}
	if strings.HasPrefix(d, ".") {
		return "", fmt.Errorf("layout: owner %q cannot be a folder name", owner)
	}
	return d, nil
}

// OwnerPath joins an owner folder and a path inside it.
func OwnerPath(owner, rel string) (string, error) {
	d, err := OwnerDir(owner)
	if err != nil {
		return "", err
	}
	return d + "/" + rel, nil
}

// SplitOwner splits a branch path into the owner (decoded) and the path inside
// its folder. Root files (.github/..., .gitlab-ci.yml) return ok=false.
func SplitOwner(p string) (owner, rel string, ok bool) {
	p = path.Clean(p)
	dir, rest, found := strings.Cut(p, "/")
	if !found || rest == "" || strings.HasPrefix(dir, ".") {
		return "", "", false
	}
	owner = DecodeName(dir)
	if enc, err := EncodeName(owner); err != nil || enc != dir {
		return "", "", false
	}
	return owner, rest, true
}

// FeatureBranch returns "feature/<owner>/<user>/<name>", all slugged.
func FeatureBranch(owner, user, name string) (string, error) {
	parts := []string{slug(owner), slug(user), slug(name)}
	for i, p := range parts {
		if p == "" {
			return "", fmt.Errorf("layout: empty %s in feature branch name", []string{"owner", "user", "name"}[i])
		}
	}
	return "feature/" + strings.Join(parts, "/"), nil
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	return strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
}
