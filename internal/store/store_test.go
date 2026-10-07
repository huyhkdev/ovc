package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileDBsAndDDLTimes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	f, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC)
	if err := f.AddDB("HR", "dev", DB{Branch: "dev"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("AddDB before Add = %v", err)
	}
	if err := f.Add(Schema{Name: "HR", Baseline: "abc", DBs: map[string]DB{"dev": {Branch: "dev"}}}); err != nil {
		t.Fatal(err)
	}
	if err := f.Add(Schema{Name: "hr", DBs: map[string]DB{"dev": {}}}); !errors.Is(err, ErrExists) {
		t.Errorf("second Add = %v", err)
	}
	if err := f.AddDB("hr", "prd", DB{Branch: "prd"}); err != nil {
		t.Fatal(err)
	}
	if err := f.AddDB("hr", "prd", DB{Branch: "prd"}); !errors.Is(err, ErrDBExists) {
		t.Errorf("AddDB twice = %v", err)
	}
	if err := f.SaveDDLTimes("HR", "prd", map[string]time.Time{"tables/T.sql": now}); err != nil {
		t.Fatal(err)
	}

	// everything survives a reopen
	g, err := OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s, ok := g.Get("Hr")
	if !ok || s.Baseline != "abc" || s.DBs["dev"].Branch != "dev" || s.DBs["prd"].Branch != "prd" {
		t.Errorf("reopened = %+v", s)
	}
	times, err := g.DDLTimes("hr", "prd")
	if err != nil || !times["tables/T.sql"].Equal(now) {
		t.Errorf("ddl times = %v, %v", times, err)
	}
	if _, err := g.DDLTimes("hr", "dev"); err == nil {
		t.Error("dev has no saved times")
	}
}

func TestBases(t *testing.T) {
	f, _ := OpenFile(filepath.Join(t.TempDir(), "state.json"))
	if b, err := f.Bases("HR", "dev"); err != nil || len(b) != 0 {
		t.Fatalf("empty bases = %v, %v", b, err)
	}
	f.SaveBases("HR", "dev", map[string]Base{"HR/a.prc": {Hash: "h1", Commit: "c1"}, "HR/b.prc": {Hash: "h2", Commit: "c1"}})
	f.SaveBases("hr", "dev", map[string]Base{"HR/a.prc": {Hash: "h3", Commit: "c2"}}) // update one, keep the other
	b, err := f.Bases("HR", "dev")
	if err != nil || b["HR/a.prc"].Hash != "h3" || b["HR/b.prc"].Commit != "c1" || len(b) != 2 {
		t.Errorf("bases = %v, %v", b, err)
	}
	if b, _ := f.Bases("HR", "prd"); len(b) != 0 {
		t.Error("bases are per database")
	}
}

func TestFileRejectsOldFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	old := `{"hr":{"name":"HR","db_alias":"dev","branches":{"dev":"dev","prd":"prd"},"baseline":"a5"}}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFile(path); err == nil || !strings.Contains(err.Error(), "older OVC") {
		t.Errorf("OpenFile(old) = %v", err)
	}
}
