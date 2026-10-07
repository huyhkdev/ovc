// Package store keeps the registry of schemas OVC manages. This first version
// is a JSON file; the spec's PostgreSQL metadata DB (§12) can replace it behind
// the same interface.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	ErrExists   = errors.New("store: schema already registered")
	ErrDBExists = errors.New("store: schema already initialised on this db")
	ErrNotFound = errors.New("store: schema not registered")
)

// Schema is one registered owner (spec §12 schema_registry). It is added to
// one database branch per `ovc init <OWNER> --db <alias>`.
type Schema struct {
	Name      string        `json:"name"`     // owner as Oracle stores it, e.g. QLSC
	Dir       string        `json:"dir"`      // its folder on every db branch
	Baseline  string        `json:"baseline"` // owner baseline: root commit merged into every db branch
	DBs       map[string]DB `json:"dbs"`      // db alias (= branch) -> what its init created
	CreatedBy string        `json:"created_by"`
	CreatedAt time.Time     `json:"created_at"`
}

// DB is the init of one owner on one database (spec §12 schema_db).
type DB struct {
	Branch    string    `json:"branch"` // = the db alias
	Commit    string    `json:"commit"` // head of the branch right after init
	Objects   int       `json:"objects"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
}

type Store interface {
	// Add registers a new owner; s.DBs holds its first database.
	Add(s Schema) error
	// AddDB registers the init of an existing owner on one more database.
	AddDB(schema, db string, d DB) error
	Get(name string) (Schema, bool)
	List() []Schema
	// SaveDDLTimes keeps the init-time last_ddl_time of every stub of one
	// owner on one database (spec §12 stub_ddl_time), keyed by branch path.
	SaveDDLTimes(schema, db string, t map[string]time.Time) error
	DDLTimes(schema, db string) (map[string]time.Time, error)
	// Bases returns the base of every object of one owner on one database
	// (spec §9.8, §12 object_base), keyed by branch path; none yet = empty.
	Bases(schema, db string) (map[string]Base, error)
	// SaveBases records new bases for some objects, keeping the others.
	SaveBases(schema, db string, b map[string]Base) error
}

// Base is the last point where the branch and the database agreed on an
// object: the hash of its DDL and the commit holding that content.
type Base struct {
	Hash   string    `json:"hash"`
	Commit string    `json:"commit"`
	At     time.Time `json:"at"`
}

func key(name string) string { return strings.ToLower(name) }

// File is a Store persisted as JSON, written atomically. DDL times, which can
// be tens of thousands of entries per owner, go to one file per owner+db next
// to the registry so registry writes stay small.
type File struct {
	path string
	mu   sync.Mutex
	data map[string]Schema
}

func OpenFile(path string) (*File, error) {
	f := &File{path: path, data: map[string]Schema{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &f.data); err != nil {
		return nil, err
	}
	for k, s := range f.data {
		if len(s.DBs) == 0 {
			return nil, fmt.Errorf("store: %s: schema %s has no dbs (state written by an older OVC; reset it and init again)", path, k)
		}
	}
	return f, nil
}

func (f *File) Add(s Schema) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, dup := f.data[key(s.Name)]; dup {
		return ErrExists
	}
	f.data[key(s.Name)] = s
	if err := f.save(); err != nil {
		delete(f.data, key(s.Name))
		return err
	}
	return nil
}

func (f *File) AddDB(schema, db string, d DB) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.data[key(schema)]
	if !ok {
		return ErrNotFound
	}
	if _, dup := s.DBs[db]; dup {
		return ErrDBExists
	}
	dbs := make(map[string]DB, len(s.DBs)+1)
	for k, v := range s.DBs {
		dbs[k] = v
	}
	dbs[db] = d
	prev := s
	s.DBs = dbs
	f.data[key(schema)] = s
	if err := f.save(); err != nil {
		f.data[key(schema)] = prev
		return err
	}
	return nil
}

func (f *File) Get(name string) (Schema, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.data[key(name)]
	return s, ok
}

func (f *File) List() []Schema {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Schema, 0, len(f.data))
	for _, s := range f.data {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return key(out[i].Name) < key(out[j].Name) })
	return out
}

func (f *File) ddlPath(schema, db string) string {
	return filepath.Join(filepath.Dir(f.path), "ddl_times", db, key(schema)+".json")
}

func (f *File) SaveDDLTimes(schema, db string, t map[string]time.Time) error {
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return writeAtomic(f.ddlPath(schema, db), b)
}

func (f *File) DDLTimes(schema, db string) (map[string]time.Time, error) {
	b, err := os.ReadFile(f.ddlPath(schema, db))
	if err != nil {
		return nil, err
	}
	var t map[string]time.Time
	return t, json.Unmarshal(b, &t)
}

func (f *File) basePath(schema, db string) string {
	return filepath.Join(filepath.Dir(f.path), "bases", db, key(schema)+".json")
}

func (f *File) Bases(schema, db string) (map[string]Base, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.readBases(schema, db)
}

func (f *File) readBases(schema, db string) (map[string]Base, error) {
	out := map[string]Base{}
	b, err := os.ReadFile(f.basePath(schema, db))
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	return out, json.Unmarshal(b, &out)
}

func (f *File) SaveBases(schema, db string, upd map[string]Base) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	all, err := f.readBases(schema, db)
	if err != nil {
		return err
	}
	for p, b := range upd {
		all[p] = b
	}
	b, err := json.Marshal(all)
	if err != nil {
		return err
	}
	return writeAtomic(f.basePath(schema, db), b)
}

func (f *File) save() error {
	b, err := json.MarshalIndent(f.data, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(f.path, b)
}

func writeAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
