package layout

import (
	"strings"
	"testing"
	"time"
)

func TestPathForAndParseRoundTrip(t *testing.T) {
	cases := []struct {
		typ  ObjectType
		name string
		path string
		kind Kind
	}{
		{PackageSpec, "PKG_EMPLOYEE", "packages/PKG_EMPLOYEE.pks", Repeatable},
		{PackageBody, "PKG_EMPLOYEE", "packages/PKG_EMPLOYEE.pkb", Repeatable},
		{TypeSpec, "T_EMP_REC", "types/T_EMP_REC.tps", Repeatable},
		{TypeBody, "T_EMP_REC", "types/T_EMP_REC.tpb", Repeatable},
		{Procedure, "P_SYNC_EMPLOYEE", "procedures/P_SYNC_EMPLOYEE.prc", Repeatable},
		{Function, "F_GET_SALARY", "functions/F_GET_SALARY.fnc", Repeatable},
		{Trigger, "TRG_EMPLOYEES_BIU", "triggers/TRG_EMPLOYEES_BIU.trg", Repeatable},
		{View, "V_EMPLOYEE_INFO", "views/V_EMPLOYEE_INFO.sql", Repeatable},
		{Synonym, "SYN_DEPT", "synonyms/SYN_DEPT.sql", Repeatable},
		{Table, "EMPLOYEES", "tables/EMPLOYEES.sql", Snapshot},
		{Index, "EMPLOYEES_IX1", "indexes/EMPLOYEES_IX1.sql", Snapshot},
		{Constraint, "EMPLOYEES", "constraints/EMPLOYEES.sql", Snapshot},
		{Sequence, "EMPLOYEES_SEQ", "sequences/EMPLOYEES_SEQ.sql", Snapshot},
		{View, "mixedCase", "views/~mixed~C~ase~.sql", Repeatable}, // quoted name: case carried by '~'
		{Function, "KiemTra_MST", "functions/K~iem~T~ra_~MST.fnc", Repeatable},
		{Procedure, "lay_dbtb_1", "procedures/~lay_dbtb_1~.prc", Repeatable},
		{Table, "A$B#1", "tables/A$B#1.sql", Snapshot}, // symbols unchanged
	}
	for _, c := range cases {
		got, err := PathFor(c.typ, c.name)
		if err != nil || got != c.path {
			t.Errorf("PathFor(%s,%s) = %q,%v; want %q", c.typ, c.name, got, err, c.path)
		}
		e, err := Parse(c.path)
		if err != nil {
			t.Errorf("Parse(%q): %v", c.path, err)
			continue
		}
		if e.Type != c.typ || e.Name != c.name || e.Kind != c.kind {
			t.Errorf("Parse(%q) = %+v; want type=%s name=%s kind=%d", c.path, e, c.typ, c.name, c.kind)
		}
	}
}

func TestParseRejects(t *testing.T) {
	bad := []string{
		"packages/PKG.sql",          // wrong extension for dir
		"packages/PKG.txt",          // unknown extension
		"misc/FOO.sql",              // unknown directory
		"FOO.sql",                   // not in a directory
		"packages/sub/PKG.pks",      // nested
		"migrations/add_email.sql",  // bad migration name
		"migrations/V2026_1__x.sql", // bad timestamp
		"grants/other.sql",          // only grants.sql allowed
		"procedures/.prc",           // no name
		"packages/../etc/PKG.pks",   // cleans to etc/PKG.pks
	}
	for _, p := range bad {
		if e, err := Parse(p); err == nil {
			t.Errorf("Parse(%q) = %+v; want error", p, e)
		}
	}
}

func TestParseMigrationAndGrants(t *testing.T) {
	e, err := Parse("migrations/V20261006_1530__add_column_email.sql")
	if err != nil || e.Type != Migration || e.Kind != Versioned || e.Name != "V20261006_1530__add_column_email" {
		t.Fatalf("migration: %+v, %v", e, err)
	}
	e, err = Parse("grants/grants.sql")
	if err != nil || e.Type != Grants || e.Kind != Repeatable {
		t.Fatalf("grants: %+v, %v", e, err)
	}
}

func TestPathForRejectsBadNames(t *testing.T) {
	for _, n := range []string{"", "a/b", `a\b`, "..", ".", "a~b", "a\x00b"} {
		if _, err := PathFor(Procedure, n); err == nil {
			t.Errorf("PathFor(%q) want error", n)
		}
	}
	if _, err := PathFor("NOPE", "X"); err == nil {
		t.Error("unknown type want error")
	}
}

func TestNewMigrationName(t *testing.T) {
	at := time.Date(2026, 10, 6, 15, 30, 0, 0, time.UTC)
	got, err := NewMigrationName(at, "Add column  email!")
	if err != nil || got != "V20261006_1530__add_column_email.sql" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := Parse("migrations/" + got); err != nil {
		t.Errorf("generated name does not parse: %v", err)
	}
	if _, err := NewMigrationName(at, "  !! "); err == nil {
		t.Error("empty description want error")
	}
}

func TestDeployOrder(t *testing.T) {
	order := []ObjectType{Synonym, TypeSpec, PackageSpec, Function, View, PackageBody, Trigger, Grants}
	prev := 0
	for _, o := range order {
		r, ok := DeployRank(o)
		if !ok || r < prev {
			t.Errorf("%s rank %d (ok=%v) breaks order after %d", o, r, ok, prev)
		}
		prev = r
	}
	if _, ok := DeployRank(Table); ok {
		t.Error("tables are snapshots and must not be deployed")
	}
	// Procedures and functions share a phase; bodies come after views.
	rp, _ := DeployRank(Procedure)
	rf, _ := DeployRank(Function)
	rv, _ := DeployRank(View)
	rb, _ := DeployRank(PackageBody)
	if rp != rf || rv >= rb {
		t.Errorf("bad ranks: proc=%d func=%d view=%d body=%d", rp, rf, rv, rb)
	}
}

func TestMatchExclude(t *testing.T) {
	pats := []string{"SYS_*", "BIN$*", "MLOG$_*", "RUPD$_*", "TMP_*", "EXACT", "A*B*C"}
	yes := []string{"SYS_C001", "BIN$abc==$0", "MLOG$_EMP", "RUPD$_EMP", "TMP_X", "EXACT", "AxxBxxC", "ABC"}
	no := []string{"PKG_EMPLOYEE", "MY_SYS_X", "EXACTLY", "AxxBxx", "SYS"}
	for _, n := range yes {
		if !MatchExclude(n, pats) {
			t.Errorf("%q should be excluded", n)
		}
	}
	for _, n := range no {
		if MatchExclude(n, pats) {
			t.Errorf("%q should not be excluded", n)
		}
	}
}

func TestStubRoundTrip(t *testing.T) {
	for _, typ := range []ObjectType{PackageBody, Table, MaterializedView, TypeBody} {
		c := RenderStub(typ)
		if !IsStub([]byte(c)) {
			t.Errorf("%s: IsStub false for %q", typ, c)
		}
		info, err := ParseStub(c)
		if err != nil || info.Type != typ {
			t.Errorf("%s: ParseStub = %+v, %v", typ, info, err)
		}
	}
	if got := RenderStub(PackageBody); got != "-- OVC:STUB type=PACKAGE_BODY\n" {
		t.Errorf("stub = %q (must not carry anything that differs between databases)", got)
	}
	if IsStub([]byte("CREATE OR REPLACE PROCEDURE X IS BEGIN NULL; END;\n/\n")) {
		t.Error("real DDL must not be a stub")
	}
	if _, err := ParseStub("-- OVC:STUB last_ddl_time=2026-10-01T10:15:00Z\n"); err == nil {
		t.Error("stub without type must fail")
	}
	if _, err := ParseStub("-- OVC:STUB type=NOPE\n"); err == nil {
		t.Error("stub with unknown type must fail")
	}
	// stubs written before last_ddl_time moved to the server still parse
	if info, err := ParseStub("-- OVC:STUB type=TABLE last_ddl_time=2026-10-01T10:15:00Z\n"); err != nil || info.Type != Table {
		t.Errorf("old stub: %+v, %v", info, err)
	}
}

func TestEncodeNameNoCaseInsensitiveCollisions(t *testing.T) {
	names := []string{"KiemTra_MST", "KIEMTRA_MST", "kiemtra_mst", "Kiemtra_MST", "KIEMtra_MST", "kIEMTRA_mst",
		"ab", "AB", "aB", "Ab", "a_b", "A_B", "a_B", "A_b", "x1y", "X1Y", "x1Y"}
	seen := map[string]string{}
	for _, n := range names {
		enc, err := EncodeName(n)
		if err != nil {
			t.Fatal(err)
		}
		if DecodeName(enc) != n {
			t.Errorf("decode(encode(%q)) = %q", n, DecodeName(enc))
		}
		key := strings.ToLower(enc)
		if prev, dup := seen[key]; dup {
			t.Errorf("%q and %q both map to %q (case-insensitively)", prev, n, enc)
		}
		seen[key] = n
	}
}

func TestBranchesAndOwners(t *testing.T) {
	for _, a := range []string{"dev", "prd", "uat-2", "db_hr"} {
		if b, err := DBBranch(a); err != nil || b != a {
			t.Errorf("DBBranch(%q) = %q, %v", a, b, err)
		}
	}
	for _, a := range []string{"", "Dev", "a b", "-x", "a..b", "x.lock", "feature/x"} {
		if _, err := DBBranch(a); err == nil {
			t.Errorf("DBBranch(%q) want error", a)
		}
	}

	for _, c := range []struct{ owner, dir string }{{"HR", "HR"}, {"QL_SC", "QL_SC"}, {"Sales", "S~ales~"}, {"A$B#", "A$B#"}} {
		d, err := OwnerDir(c.owner)
		if err != nil || d != c.dir {
			t.Errorf("OwnerDir(%q) = %q, %v; want %q", c.owner, d, err, c.dir)
		}
		p, _ := OwnerPath(c.owner, "packages/P.pkb")
		o, rel, ok := SplitOwner(p)
		if !ok || o != c.owner || rel != "packages/P.pkb" {
			t.Errorf("SplitOwner(%q) = %q,%q,%v", p, o, rel, ok)
		}
	}
	for _, o := range []string{"", "a/b", "x~y", ".hidden", ".."} {
		if _, err := OwnerDir(o); err == nil {
			t.Errorf("OwnerDir(%q) want error", o)
		}
	}
	for _, p := range []string{".github/workflows/ovc-deploy.yml", ".gitlab-ci.yml", "HR", "Sales/x.sql"} {
		if o, rel, ok := SplitOwner(p); ok {
			t.Errorf("SplitOwner(%q) = %q,%q; want not an owner path", p, o, rel)
		}
	}

	f, err := FeatureBranch("QLSC", "Nguyen Van A", "Add column Email!")
	if err != nil || f != "feature/qlsc/nguyen-van-a/add-column-email" {
		t.Errorf("FeatureBranch = %q, %v", f, err)
	}
	if _, err := FeatureBranch("qlsc", "!!!", "x"); err == nil {
		t.Error("empty user slug must be rejected")
	}
}
