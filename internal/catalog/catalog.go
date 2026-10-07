// Package catalog is the server's read-only view of Oracle schemas.
package catalog

import (
	"context"
	"database/sql"
	"fmt"

	"ovc/internal/config"
	"ovc/internal/oracle/export"
)

type Catalog interface {
	HasAlias(alias string) bool
	// Objects lists the managed objects of schema in the DB behind alias.
	Objects(ctx context.Context, alias, schema string, exclude []string) ([]export.Object, error)
	// Hydrate reads the normalised DDL of objs (spec §9.8). Per-object
	// failures are in File.Err; the error is for the connection as a whole.
	Hydrate(ctx context.Context, alias, schema string, objs []export.Object, workers int) ([]export.File, error)
}

// Oracle implements Catalog with the read-only account of each alias.
type Oracle struct{ DBs map[string]config.Database }

func (o Oracle) HasAlias(alias string) bool { _, ok := o.DBs[alias]; return ok }

func (o Oracle) open(ctx context.Context, alias string) (*sql.DB, error) {
	d, ok := o.DBs[alias]
	if !ok {
		return nil, fmt.Errorf("unknown db alias %q", alias)
	}
	cfg, err := d.ExportConfig()
	if err != nil {
		return nil, err
	}
	return export.Open(ctx, cfg)
}

func (o Oracle) Objects(ctx context.Context, alias, schema string, exclude []string) ([]export.Object, error) {
	db, err := o.open(ctx, alias)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	return export.List(ctx, db, schema, exclude)
}

func (o Oracle) Hydrate(ctx context.Context, alias, schema string, objs []export.Object, workers int) ([]export.File, error) {
	db, err := o.open(ctx, alias)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(workers)
	return export.Hydrate(ctx, db, schema, objs, workers), nil
}
