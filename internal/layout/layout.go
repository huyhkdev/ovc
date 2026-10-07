// Package layout defines the schema repo layout (spec §5): which directory and
// file extension each Oracle object type uses, how migrations are named, and
// the order in which repeatable objects are deployed.
package layout

import (
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode"
)

type ObjectType string

const (
	Table            ObjectType = "TABLE"
	Index            ObjectType = "INDEX"
	Constraint       ObjectType = "CONSTRAINT"
	Sequence         ObjectType = "SEQUENCE"
	View             ObjectType = "VIEW"
	MaterializedView ObjectType = "MATERIALIZED VIEW"
	TypeSpec         ObjectType = "TYPE"
	TypeBody         ObjectType = "TYPE BODY"
	PackageSpec      ObjectType = "PACKAGE"
	PackageBody      ObjectType = "PACKAGE BODY"
	Procedure        ObjectType = "PROCEDURE"
	Function         ObjectType = "FUNCTION"
	Trigger          ObjectType = "TRIGGER"
	Synonym          ObjectType = "SYNONYM"
	Grants           ObjectType = "GRANTS"
	Migration        ObjectType = "MIGRATION"
)

// Kind tells how a file is deployed (spec §5.3).
type Kind int

const (
	// Repeatable files are re-run (CREATE OR REPLACE) whenever they change.
	Repeatable Kind = iota
	// Versioned files run exactly once and must never be edited afterwards.
	Versioned
	// Snapshot files describe current structure; the pipeline does not run them.
	Snapshot
)

type spec struct {
	dir  string
	ext  string
	kind Kind
}

var specs = map[ObjectType]spec{
	Table:            {"tables", ".sql", Snapshot},
	Index:            {"indexes", ".sql", Snapshot},
	Constraint:       {"constraints", ".sql", Snapshot},
	Sequence:         {"sequences", ".sql", Snapshot},
	View:             {"views", ".sql", Repeatable},
	MaterializedView: {"materialized_views", ".sql", Repeatable},
	TypeSpec:         {"types", ".tps", Repeatable},
	TypeBody:         {"types", ".tpb", Repeatable},
	PackageSpec:      {"packages", ".pks", Repeatable},
	PackageBody:      {"packages", ".pkb", Repeatable},
	Procedure:        {"procedures", ".prc", Repeatable},
	Function:         {"functions", ".fnc", Repeatable},
	Trigger:          {"triggers", ".trg", Repeatable},
	Synonym:          {"synonyms", ".sql", Repeatable},
	Grants:           {"grants", ".sql", Repeatable},
	Migration:        {"migrations", ".sql", Versioned},
}

// Entry is the result of classifying a repo-relative path.
type Entry struct {
	Type ObjectType
	Name string // object name as Oracle stores it; for Migration, the file name without extension
	Kind Kind
	Path string
}

// Dir returns the directory that holds objects of type t.
func Dir(t ObjectType) string { return specs[t].dir }

// Ext returns the file extension (with dot) used for objects of type t.
func Ext(t ObjectType) string { return specs[t].ext }

// KindOf returns how objects of type t are deployed.
func KindOf(t ObjectType) Kind { return specs[t].kind }

// PathFor returns the repo-relative path of an object. Names are kept exactly
// as given (Oracle's case, including quoted lowercase names).
func PathFor(t ObjectType, name string) (string, error) {
	s, ok := specs[t]
	if !ok {
		return "", fmt.Errorf("layout: unknown object type %q", t)
	}
	stem, err := EncodeName(name)
	if err != nil {
		return "", err
	}
	return path.Join(s.dir, stem+s.ext), nil
}

// EncodeName turns an Oracle object name into a file name stem (spec §5.1).
//
// Oracle treats "KiemTra_MST" and KIEMTRA_MST as different objects, but they
// would be the same file on case-insensitive file systems (macOS, Windows).
// Names without lowercase letters (the usual case) are used unchanged. For the
// others, every run of lowercase letters is wrapped in '~' so that case is
// carried by the '~' positions and no two names can collide when folded:
//
//	KiemTra_MST -> K~iem~T~ra_~MST        lay_dbtb -> ~lay_dbtb~
//
// Non-letters do not change the mode. DecodeName reverses it by dropping '~'.
// Names containing '~', path separators or control characters are rejected.
func EncodeName(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("layout: empty object name")
	}
	if name == "." || name == ".." || strings.ContainsAny(name, "/\\~") {
		return "", fmt.Errorf("layout: invalid object name %q", name)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("layout: object name %q has control characters", name)
		}
	}
	var b strings.Builder
	lower := false
	for _, r := range name {
		switch {
		case unicode.IsLower(r):
			if !lower {
				b.WriteByte('~')
				lower = true
			}
		case unicode.IsUpper(r):
			if lower {
				b.WriteByte('~')
				lower = false
			}
		}
		b.WriteRune(r)
	}
	if lower {
		b.WriteByte('~')
	}
	return b.String(), nil
}

// DecodeName reverses EncodeName.
func DecodeName(stem string) string { return strings.ReplaceAll(stem, "~", "") }

// Parse classifies a repo-relative, slash-separated path. Files outside the
// managed directories (ovc.yaml, .gitlab-ci.yml, ...) return an error; callers
// that accept those should check for them first with IsRepoFile.
func Parse(p string) (Entry, error) {
	p = path.Clean(p)
	dir, file := path.Split(p)
	dir = strings.TrimSuffix(dir, "/")
	if dir == "" || strings.Contains(dir, "/") {
		return Entry{}, fmt.Errorf("layout: %q is not directly inside a managed directory", p)
	}
	ext := path.Ext(file)
	name := strings.TrimSuffix(file, ext)
	if name == "" {
		return Entry{}, fmt.Errorf("layout: %q has no object name", p)
	}
	if dir == "grants" {
		// grants/ holds a single grants.sql (spec §5).
		if file != "grants.sql" {
			return Entry{}, fmt.Errorf("layout: only grants/grants.sql is allowed, got %q", p)
		}
		return Entry{Type: Grants, Name: "grants", Kind: Repeatable, Path: p}, nil
	}
	if dir == "migrations" {
		if ext != ".sql" || !migrationRe.MatchString(file) {
			return Entry{}, fmt.Errorf("layout: %q is not a valid migration name (want V<yyyyMMdd>_<HHmm>__<desc>.sql)", p)
		}
		return Entry{Type: Migration, Name: name, Kind: Versioned, Path: p}, nil
	}
	for t, s := range specs {
		if s.dir == dir && s.ext == ext && t != Grants && t != Migration {
			real := DecodeName(name)
			if enc, err := EncodeName(real); err != nil || enc != name {
				return Entry{}, fmt.Errorf("layout: %q is not a canonical file name for object %q (want %q)", p, real, enc)
			}
			return Entry{Type: t, Name: real, Kind: s.kind, Path: p}, nil
		}
	}
	return Entry{}, fmt.Errorf("layout: %q has no matching object type (directory %q, extension %q)", p, dir, ext)
}

// IsRepoFile reports whether p is a non-object file that is allowed at the
// repo root.
func IsRepoFile(p string) bool {
	switch path.Clean(p) {
	case "ovc.yaml", ".gitlab-ci.yml", ".gitignore", "README.md":
		return true
	}
	return false
}

var migrationRe = regexp.MustCompile(`^V\d{8}_\d{4}__[A-Za-z0-9_]+\.sql$`)

var descRe = regexp.MustCompile(`[^A-Za-z0-9]+`)

// NewMigrationName builds a migration file name from a timestamp and a free
// text description: "add column email" -> V20261006_1530__add_column_email.sql.
func NewMigrationName(t time.Time, desc string) (string, error) {
	d := strings.Trim(descRe.ReplaceAllString(strings.ToLower(desc), "_"), "_")
	if d == "" {
		return "", fmt.Errorf("layout: migration description is empty")
	}
	return fmt.Sprintf("V%s__%s.sql", t.Format("20060102_1504"), d), nil
}

// deployOrder follows spec §10.4 step 5: synonyms, type spec, package spec,
// functions/procedures, views, type/package bodies, triggers, grants. Lower
// runs first. Sequences are listed in §10.4 but are snapshot/migration objects
// per §5.3, so they are not part of this order (see docs/spec.md open issues).
var deployOrder = map[ObjectType]int{
	Synonym:          10,
	TypeSpec:         20,
	PackageSpec:      30,
	Function:         40,
	Procedure:        40,
	View:             50,
	MaterializedView: 50,
	TypeBody:         60,
	PackageBody:      60,
	Trigger:          70,
	Grants:           80,
}

// DeployRank returns the position of t in the repeatable deploy order.
// Types that are not deployed in that phase return 0, false.
func DeployRank(t ObjectType) (int, bool) {
	r, ok := deployOrder[t]
	return r, ok
}

// MatchExclude reports whether an object name matches any exclude pattern from
// ovc.yaml. Patterns support "*" as a wildcard for any run of characters.
func MatchExclude(name string, patterns []string) bool {
	for _, p := range patterns {
		if globMatch(p, name) {
			return true
		}
	}
	return false
}

func globMatch(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	last := parts[len(parts)-1]
	for _, mid := range parts[1 : len(parts)-1] {
		i := strings.Index(s, mid)
		if i < 0 {
			return false
		}
		s = s[i+len(mid):]
	}
	return strings.HasSuffix(s, last)
}
