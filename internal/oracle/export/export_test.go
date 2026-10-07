package export

import (
	"strings"
	"testing"
	"time"

	"ovc/internal/layout"
)

func TestNormalize(t *testing.T) {
	cases := []struct {
		name string
		typ  layout.ObjectType
		in   string
		want string
	}{
		{"package keeps body and slash, drops EDITIONABLE and indent",
			layout.PackageSpec,
			"\n  CREATE OR REPLACE EDITIONABLE PACKAGE \"PKG\" AS\n  PROCEDURE p;\nEND PKG;\n/",
			"CREATE OR REPLACE PACKAGE \"PKG\" AS\n  PROCEDURE p;\nEND PKG;\n/\n"},
		{"view FORCE EDITIONABLE",
			layout.View,
			"\n  CREATE OR REPLACE FORCE EDITIONABLE VIEW \"V\" (\"ID\") AS \n  SELECT ID FROM T;",
			"CREATE OR REPLACE FORCE VIEW \"V\" (\"ID\") AS\n  SELECT ID FROM T;\n"},
		{"NONEDITIONABLE is kept",
			layout.Procedure,
			"CREATE OR REPLACE NONEDITIONABLE PROCEDURE \"P\" IS BEGIN NULL; END;\n/",
			"CREATE OR REPLACE NONEDITIONABLE PROCEDURE \"P\" IS BEGIN NULL; END;\n/\n"},
		{"index with dangling semicolon and CRLF",
			layout.Index,
			"\r\n  CREATE INDEX \"IX\" ON \"T\" (\"NAME\") \r\n  ;",
			"CREATE INDEX \"IX\" ON \"T\" (\"NAME\");\n"},
		{"sequence START WITH is removed",
			layout.Sequence,
			"\n   CREATE SEQUENCE  \"S\"  MINVALUE 1 MAXVALUE 99 INCREMENT BY 1 START WITH 4321 CACHE 20 NOORDER  NOCYCLE ;",
			"CREATE SEQUENCE  \"S\"  MINVALUE 1 MAXVALUE 99 INCREMENT BY 1 CACHE 20 NOORDER  NOCYCLE;\n"},
		{"START WITH in a view is untouched",
			layout.View,
			"CREATE OR REPLACE VIEW \"V\" AS SELECT 'START WITH 5' X FROM DUAL CONNECT BY LEVEL < 2 START WITH 1 = 1;",
			"CREATE OR REPLACE VIEW \"V\" AS SELECT 'START WITH 5' X FROM DUAL CONNECT BY LEVEL < 2 START WITH 1 = 1;\n"},
		{"trigger keeps trailing ALTER",
			layout.Trigger,
			"\n  CREATE OR REPLACE EDITIONABLE TRIGGER \"TRG\" BEFORE INSERT ON T BEGIN NULL; END;\n/\nALTER TRIGGER \"TRG\" ENABLE;",
			"CREATE OR REPLACE TRIGGER \"TRG\" BEFORE INSERT ON T BEGIN NULL; END;\n/\nALTER TRIGGER \"TRG\" ENABLE;\n"},
	}
	for _, c := range cases {
		got := Normalize(c.typ, c.in)
		if got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
		if again := Normalize(c.typ, got); again != got {
			t.Errorf("%s: not idempotent: %q -> %q", c.name, got, again)
		}
	}
}

func TestBuildShell(t *testing.T) {
	ts := time.Date(2026, 10, 1, 10, 15, 0, 0, time.UTC)
	objs := []Object{
		{Type: layout.PackageSpec, Name: "PKG_EMPLOYEE", LastDDLTime: ts},
		{Type: layout.PackageBody, Name: "PKG_EMPLOYEE", LastDDLTime: ts},
		{Type: layout.Table, Name: "EMPLOYEES", LastDDLTime: ts},
		{Type: layout.Constraint, Name: "EMPLOYEES", LastDDLTime: ts},
		{Type: layout.View, Name: "abc", LastDDLTime: ts},
		{Type: layout.View, Name: "ABC", LastDDLTime: ts}, // differs from "abc" only by case: both are kept
		{Type: layout.Procedure, Name: "A/B", LastDDLTime: ts},
		{Type: layout.Procedure, Name: "a~b", LastDDLTime: ts},
	}
	sh := BuildShell(objs)
	for _, p := range []string{"packages/PKG_EMPLOYEE.pks", "packages/PKG_EMPLOYEE.pkb", "tables/EMPLOYEES.sql", "constraints/EMPLOYEES.sql", "views/~abc~.sql", "views/ABC.sql"} {
		c, ok := sh.Files[p]
		if !ok {
			t.Errorf("missing %s", p)
			continue
		}
		if !layout.IsStub([]byte(c)) {
			t.Errorf("%s is not a stub: %q", p, c)
		}
		if _, err := layout.Parse(p); err != nil {
			t.Errorf("%s does not parse back: %v", p, err)
		}
	}
	if len(sh.Files) != 6 {
		t.Errorf("want 6 files, got %d: %v", len(sh.Files), sh.Files)
	}
	if len(sh.DDLTimes) != len(sh.Files) || !sh.DDLTimes["tables/EMPLOYEES.sql"].Equal(ts) {
		t.Errorf("DDLTimes = %v", sh.DDLTimes)
	}
	if len(sh.Warnings) != 2 {
		t.Errorf("want 2 warnings (names with '/' and '~'), got %v", sh.Warnings)
	}
	lower := map[string]bool{}
	for p := range sh.Files {
		if lower[strings.ToLower(p)] {
			t.Errorf("%s collides on case-insensitive file systems", p)
		}
		lower[strings.ToLower(p)] = true
	}
}

func TestRelated(t *testing.T) {
	all := []Object{
		{Type: layout.Table, Name: "EMP"},
		{Type: layout.Index, Name: "EMP_IX1", Parent: "EMP"},
		{Type: layout.Index, Name: "DEPT_IX1", Parent: "DEPT"},
		{Type: layout.Constraint, Name: "EMP"},
		{Type: layout.Constraint, Name: "DEPT"},
	}
	got := Related(all, "EMP")
	if len(got) != 2 || got[0].Name != "EMP_IX1" || got[1].Type != layout.Constraint {
		t.Errorf("Related = %+v", got)
	}
}

func TestDedupe(t *testing.T) {
	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)
	in := []Object{
		{Type: layout.TypeSpec, Name: "T", LastDDLTime: t1},
		{Type: layout.TypeSpec, Name: "T", LastDDLTime: t2},
		{Type: layout.TypeSpec, Name: "T", LastDDLTime: t1},
		{Type: layout.TypeBody, Name: "T", LastDDLTime: t1}, // same name, different type: kept
		{Type: layout.Table, Name: "t", LastDDLTime: t1},    // different case: kept (Oracle treats it as another object)
	}
	got := dedupe(in)
	if len(got) != 3 {
		t.Fatalf("want 3, got %d: %+v", len(got), got)
	}
	if !got[0].LastDDLTime.Equal(t2) {
		t.Errorf("newest last_ddl_time not kept: %v", got[0].LastDDLTime)
	}
}
