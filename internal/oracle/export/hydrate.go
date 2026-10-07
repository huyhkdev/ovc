package export

import (
	"context"
	"database/sql"
	"sync"

	"ovc/internal/layout"
)

// File is one hydrated object.
type File struct {
	Object  Object
	Path    string
	Content string // normalised DDL
	Err     error
}

// Hydrate reads the real DDL of objs (spec §9.8). It uses up to workers
// sessions in parallel; results keep the order of objs. A failure on one object
// is reported in its File.Err and does not stop the others.
func Hydrate(ctx context.Context, db *sql.DB, schema string, objs []Object, workers int) []File {
	if workers <= 0 {
		workers = 1
	}
	if workers > len(objs) {
		workers = len(objs)
	}
	out := make([]File, len(objs))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sess, err := NewSession(ctx, db, schema)
			for i := range jobs {
				out[i] = File{Object: objs[i]}
				if err != nil {
					out[i].Err = err
					continue
				}
				out[i].Path, out[i].Err = layout.PathFor(objs[i].Type, objs[i].Name)
				if out[i].Err != nil {
					continue
				}
				ddl, derr := sess.DDL(ctx, objs[i])
				if derr != nil {
					out[i].Err = derr
					continue
				}
				out[i].Content = Normalize(objs[i].Type, ddl)
			}
			if sess != nil {
				sess.Close()
			}
		}()
	}
	for i := range objs {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return out
}

// Related returns what `ovc get` pulls together with a table: its indexes and
// the foreign-key file grouped under the same table name (spec §5.1, §9.8).
func Related(all []Object, table string) []Object {
	var out []Object
	for _, o := range all {
		switch {
		case o.Type == layout.Index && o.Parent == table:
			out = append(out, o)
		case o.Type == layout.Constraint && o.Name == table:
			out = append(out, o)
		}
	}
	return out
}
