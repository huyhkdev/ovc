package export

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"

	"ovc/internal/layout"
)

// Needs the fixture database: deploy/dev-oracle.sh && source .ovc-dev.env
func itDB(t *testing.T) (Config, string) {
	t.Helper()
	if os.Getenv("OVC_IT_HOST") == "" {
		t.Skip("OVC_IT_* not set (run deploy/dev-oracle.sh and source .ovc-dev.env)")
	}
	port, _ := strconv.Atoi(os.Getenv("OVC_IT_PORT"))
	return Config{
		Host: os.Getenv("OVC_IT_HOST"), Port: port, Service: os.Getenv("OVC_IT_SERVICE"),
		User: os.Getenv("OVC_IT_USER"), Password: os.Getenv("OVC_IT_PASSWORD"),
	}, os.Getenv("OVC_IT_SCHEMA")
}

func TestIntegrationListHydrate(t *testing.T) {
	cfg, schema := itDB(t)
	ctx := context.Background()
	db, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// P_OVC_E2E* belong to the CLI end-to-end test, which edits them while
	// this test may run in parallel against the same database.
	objs, err := List(ctx, db, schema, []string{"TMP_*", "P_OVC_E2E*"})
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]Object{}
	for _, o := range objs {
		have[string(o.Type)+":"+o.Name] = o
	}
	for _, k := range []string{
		"PACKAGE:PKG_EMPLOYEE", "PACKAGE BODY:PKG_EMPLOYEE", "TABLE:EMPLOYEES", "TABLE:DEPARTMENTS",
		"INDEX:EMPLOYEES_IX1", "SEQUENCE:EMPLOYEES_SEQ", "VIEW:V_EMPLOYEE_INFO", "MATERIALIZED VIEW:MV_EMP_COUNT",
		"TYPE:T_EMP_REC", "PROCEDURE:P_SYNC_EMPLOYEE", "FUNCTION:F_GET_SALARY", "TRIGGER:TRG_EMPLOYEES_BIU",
		"SYNONYM:SYN_DEPT", "CONSTRAINT:EMPLOYEES",
	} {
		if _, ok := have[k]; !ok {
			t.Errorf("missing %s", k)
		}
	}
	if _, ok := have["TABLE:TMP_SCRATCH"]; ok {
		t.Error("TMP_* should be excluded")
	}
	if _, ok := have["TABLE:MV_EMP_COUNT"]; ok {
		t.Error("materialized view container table must not be listed as TABLE")
	}
	if p := have["INDEX:EMPLOYEES_IX1"].Parent; p != "EMPLOYEES" {
		t.Errorf("index parent = %q", p)
	}
	if o := have["PACKAGE:PKG_EMPLOYEE"]; o.LastDDLTime.IsZero() {
		t.Error("last_ddl_time not read")
	}
	for _, o := range objs {
		if strings.HasPrefix(o.Name, "SYS_") || strings.HasPrefix(o.Name, "BIN$") {
			t.Errorf("system object listed: %s %s", o.Type, o.Name)
		}
	}

	// Shell: every object becomes a stub file that parses back.
	sh := BuildShell(objs)
	if len(sh.Warnings) != 0 {
		t.Errorf("warnings: %v", sh.Warnings)
	}
	for p, c := range sh.Files {
		if !layout.IsStub([]byte(c)) {
			t.Errorf("%s not a stub", p)
		}
	}

	// Hydrate two runs, parallel: content must be identical (drift relies on it).
	first := Hydrate(ctx, db, schema, objs, 4)
	second := Hydrate(ctx, db, schema, objs, 2)
	for i, f := range first {
		if f.Err != nil {
			t.Errorf("%s %s: %v", f.Object.Type, f.Object.Name, f.Err)
			continue
		}
		if second[i].Content != f.Content {
			t.Errorf("%s %s: two runs differ", f.Object.Type, f.Object.Name)
		}
		if layout.IsStub([]byte(f.Content)) || strings.TrimSpace(f.Content) == "" {
			t.Errorf("%s: bad content %q", f.Path, f.Content)
		}
		if strings.Contains(f.Content, "\r") || strings.Contains(f.Content, " \n") || strings.HasPrefix(f.Content, "\n") || !strings.HasSuffix(f.Content, "\n") {
			t.Errorf("%s: not normalised: %q", f.Path, f.Content)
		}
		if _, err := layout.Parse(f.Path); err != nil {
			t.Errorf("%s: %v", f.Path, err)
		}
		if f.Object.Type == layout.Sequence && strings.Contains(f.Content, "START WITH") {
			t.Errorf("sequence still has START WITH: %q", f.Content)
		}
		if strings.Contains(f.Content, "EDITIONABLE ") && !strings.Contains(f.Content, "NONEDITIONABLE") {
			t.Errorf("%s: EDITIONABLE not stripped", f.Path)
		}
	}

	// Related pulls index + FK file of a table.
	rel := Related(objs, "EMPLOYEES")
	if len(rel) != 2 {
		t.Errorf("Related(EMPLOYEES) = %+v", rel)
	}
}

// GET_DDL prints the sequence's current value in START WITH. Consuming values
// (data, not structure) must not change the exported file, or drift check would
// flag every sequence all the time.
func TestIntegrationSequenceStableAcrossNextval(t *testing.T) {
	cfg, schema := itDB(t)
	ownerPW := os.Getenv("OVC_IT_OWNER_PASSWORD")
	if ownerPW == "" {
		t.Skip("OVC_IT_OWNER_PASSWORD not set")
	}
	ctx := context.Background()
	reader, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	ownerCfg := cfg
	ownerCfg.User, ownerCfg.Password = schema, ownerPW
	owner, err := Open(ctx, ownerCfg)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()

	obj := []Object{{Type: layout.Sequence, Name: "EMPLOYEES_SEQ"}}
	rawOf := func() (string, string) {
		s, err := NewSession(ctx, reader, schema)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		raw, err := s.DDL(ctx, obj[0])
		if err != nil {
			t.Fatal(err)
		}
		return raw, Normalize(layout.Sequence, raw)
	}
	rawBefore, normBefore := rawOf()
	// Draw values until the dictionary high-water mark moves. The cache is 20,
	// but Oracle 23ai grows a busy sequence's cache on its own, so a fixed
	// count is not enough.
	rawAfter, normAfter := rawBefore, normBefore
	for round := 0; round < 40 && rawAfter == rawBefore; round++ {
		for i := 0; i < 250; i++ {
			var n int
			if err := owner.QueryRowContext(ctx, "SELECT EMPLOYEES_SEQ.NEXTVAL FROM dual").Scan(&n); err != nil {
				t.Fatal(err)
			}
		}
		rawAfter, normAfter = rawOf()
	}
	if rawBefore == rawAfter {
		t.Fatalf("raw DDL did not change after NEXTVAL, so this test proves nothing: %q", rawAfter)
	}
	if normBefore != normAfter {
		t.Errorf("normalised DDL changed with the sequence value:\n%q\n%q", normBefore, normAfter)
	}
}
