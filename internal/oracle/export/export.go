// Package export reads object metadata and DDL from Oracle with a read-only
// account (spec §9.1, §9.8). It never writes to the database.
package export

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	go_ora "github.com/sijms/go-ora/v2"

	"ovc/internal/layout"
)

// Object is one entry of a schema. For Constraint, Name is the table whose
// foreign keys are grouped in constraints/<TABLE>.sql (spec §5.1).
type Object struct {
	Type        layout.ObjectType
	Name        string
	Parent      string // for Index: the table it belongs to
	LastDDLTime time.Time
}

// Config is a read-only connection (OVC_READER).
type Config struct {
	Host, Service, User, Password string
	Port                          int
}

func Open(ctx context.Context, c Config) (*sql.DB, error) {
	if c.Port == 0 {
		c.Port = 1521
	}
	db, err := sql.Open("oracle", go_ora.BuildUrl(c.Host, c.Port, c.Service, c.User, c.Password, nil))
	if err != nil {
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect %s@%s:%d/%s: %w", c.User, c.Host, c.Port, c.Service, err)
	}
	return db, nil
}

// oracleTypes maps DBA_OBJECTS.OBJECT_TYPE to layout types.
var oracleTypes = map[string]layout.ObjectType{
	"TABLE":             layout.Table,
	"INDEX":             layout.Index,
	"SEQUENCE":          layout.Sequence,
	"VIEW":              layout.View,
	"MATERIALIZED VIEW": layout.MaterializedView,
	"TYPE":              layout.TypeSpec,
	"TYPE BODY":         layout.TypeBody,
	"PACKAGE":           layout.PackageSpec,
	"PACKAGE BODY":      layout.PackageBody,
	"PROCEDURE":         layout.Procedure,
	"FUNCTION":          layout.Function,
	"TRIGGER":           layout.Trigger,
	"SYNONYM":           layout.Synonym,
}

// metadataType is the DBMS_METADATA object type name.
func metadataType(t layout.ObjectType) (string, error) {
	switch t {
	case layout.PackageSpec:
		return "PACKAGE_SPEC", nil
	case layout.PackageBody:
		return "PACKAGE_BODY", nil
	case layout.TypeSpec:
		return "TYPE_SPEC", nil
	case layout.TypeBody:
		return "TYPE_BODY", nil
	case layout.MaterializedView:
		return "MATERIALIZED_VIEW", nil
	case layout.Table, layout.Index, layout.Sequence, layout.View,
		layout.Procedure, layout.Function, layout.Trigger, layout.Synonym:
		return string(t), nil
	}
	return "", fmt.Errorf("export: no DBMS_METADATA type for %s", t)
}

// List returns every managed object of schema (one query, no DBMS_METADATA).
// Objects matching exclude patterns (ovc.yaml) are dropped. It reads
// DBA_OBJECTS and falls back to ALL_OBJECTS for accounts without dictionary
// access.
func List(ctx context.Context, db *sql.DB, schema string, exclude []string) ([]Object, error) {
	objs, err := listFrom(ctx, db, "dba", schema)
	if err != nil {
		var err2 error
		if objs, err2 = listFrom(ctx, db, "all", schema); err2 != nil {
			return nil, fmt.Errorf("list objects: %w (dba_objects: %v)", err2, err)
		}
	}
	deduped := dedupe(objs)
	out := make([]Object, 0, len(deduped))
	for _, o := range deduped {
		if !layoutExcluded(o.Name, exclude) {
			out = append(out, o)
		}
	}
	return out, nil
}

// dedupe keeps one entry per (type, name), with the newest last_ddl_time.
// DBA_OBJECTS can list the same type several times (e.g. evolved object types),
// and a repeated path would make two objects fight over one file.
func dedupe(objs []Object) []Object {
	idx := map[string]int{}
	out := make([]Object, 0, len(objs))
	for _, o := range objs {
		k := string(o.Type) + "\x00" + o.Name
		if i, ok := idx[k]; ok {
			if o.LastDDLTime.After(out[i].LastDDLTime) {
				out[i].LastDDLTime = o.LastDDLTime
			}
			continue
		}
		idx[k] = len(out)
		out = append(out, o)
	}
	return out
}

func layoutExcluded(name string, exclude []string) bool {
	return layout.MatchExclude(name, exclude)
}

func listFrom(ctx context.Context, db *sql.DB, prefix, schema string) ([]Object, error) {
	schema = strings.ToUpper(schema)
	types := make([]string, 0, len(oracleTypes))
	for t := range oracleTypes {
		types = append(types, "'"+t+"'")
	}
	// A materialized view shows up twice (its container TABLE too): keep the MV only.
	// GENERATED='Y' drops system-named indexes/LOB segments etc.
	q := fmt.Sprintf(`
		SELECT object_type, object_name, last_ddl_time FROM %[1]s_objects o
		 WHERE owner = :1
		   AND object_type IN (%[2]s)
		   AND generated = 'N'
		   AND NOT (object_type = 'TABLE' AND object_name IN
		        (SELECT mview_name FROM %[1]s_mviews WHERE owner = :2))
		 ORDER BY object_type, object_name`, prefix, strings.Join(types, ","))
	rows, err := db.QueryContext(ctx, q, schema, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Object
	for rows.Next() {
		var typ, name string
		var ts sql.NullTime
		if err := rows.Scan(&typ, &name, &ts); err != nil {
			return nil, err
		}
		out = append(out, Object{Type: oracleTypes[typ], Name: name, LastDDLTime: ts.Time.UTC()})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Indexes remember their table so `ovc get TABLE` can pull them together.
	ix, err := db.QueryContext(ctx, fmt.Sprintf(
		`SELECT index_name, table_name FROM %s_indexes WHERE owner = :1`, prefix), schema)
	if err != nil {
		return nil, err
	}
	parents := map[string]string{}
	for ix.Next() {
		var name, table string
		if err := ix.Scan(&name, &table); err != nil {
			ix.Close()
			return nil, err
		}
		parents[name] = table
	}
	ix.Close()
	for i := range out {
		if out[i].Type == layout.Index {
			out[i].Parent = parents[out[i].Name]
		}
	}

	// Foreign keys are exported per table into constraints/ (spec §5.1).
	fk, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT table_name, MAX(last_change) FROM %s_constraints
		 WHERE owner = :1 AND constraint_type = 'R'
		 GROUP BY table_name ORDER BY table_name`, prefix), schema)
	if err != nil {
		return nil, err
	}
	defer fk.Close()
	for fk.Next() {
		var table string
		var ts sql.NullTime
		if err := fk.Scan(&table, &ts); err != nil {
			return nil, err
		}
		out = append(out, Object{Type: layout.Constraint, Name: table, LastDDLTime: ts.Time.UTC()})
	}
	return out, fk.Err()
}

// Session is a dedicated connection with the DBMS_METADATA transforms of spec
// §9.1/§9.8 applied. Create one per worker; transforms are session scoped.
type Session struct {
	conn   *sql.Conn
	schema string
}

func NewSession(ctx context.Context, db *sql.DB, schema string) (*Session, error) {
	c, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	_, err = c.ExecContext(ctx, `BEGIN
	  DBMS_METADATA.SET_TRANSFORM_PARAM(DBMS_METADATA.SESSION_TRANSFORM,'SEGMENT_ATTRIBUTES',FALSE);
	  DBMS_METADATA.SET_TRANSFORM_PARAM(DBMS_METADATA.SESSION_TRANSFORM,'STORAGE',FALSE);
	  DBMS_METADATA.SET_TRANSFORM_PARAM(DBMS_METADATA.SESSION_TRANSFORM,'TABLESPACE',FALSE);
	  DBMS_METADATA.SET_TRANSFORM_PARAM(DBMS_METADATA.SESSION_TRANSFORM,'EMIT_SCHEMA',FALSE);
	  DBMS_METADATA.SET_TRANSFORM_PARAM(DBMS_METADATA.SESSION_TRANSFORM,'SQLTERMINATOR',TRUE);
	  DBMS_METADATA.SET_TRANSFORM_PARAM(DBMS_METADATA.SESSION_TRANSFORM,'PRETTY',TRUE);
	  DBMS_METADATA.SET_TRANSFORM_PARAM(DBMS_METADATA.SESSION_TRANSFORM,'CONSTRAINTS_AS_ALTER',TRUE);
	  DBMS_METADATA.SET_TRANSFORM_PARAM(DBMS_METADATA.SESSION_TRANSFORM,'REF_CONSTRAINTS',FALSE);
	END;`)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("set metadata transforms: %w", err)
	}
	return &Session{conn: c, schema: strings.ToUpper(schema)}, nil
}

func (s *Session) Close() error { return s.conn.Close() }

// DDL returns the raw DBMS_METADATA text for o (not yet normalised).
func (s *Session) DDL(ctx context.Context, o Object) (string, error) {
	var ddl string
	if o.Type == layout.Constraint {
		err := s.conn.QueryRowContext(ctx,
			`SELECT DBMS_METADATA.GET_DEPENDENT_DDL('REF_CONSTRAINT', :1, :2) FROM dual`,
			o.Name, s.schema).Scan(&ddl)
		return ddl, err
	}
	mt, err := metadataType(o.Type)
	if err != nil {
		return "", err
	}
	err = s.conn.QueryRowContext(ctx, `SELECT DBMS_METADATA.GET_DDL(:1, :2, :3) FROM dual`,
		mt, o.Name, s.schema).Scan(&ddl)
	return ddl, err
}
