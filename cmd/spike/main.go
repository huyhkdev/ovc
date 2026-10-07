// Command spike checks the technical assumptions of the OVC spec against a real
// Oracle 19c database. It is a throw-away tool (not part of the product).
//
//	ddl   GET_DDL output with the §9.1 transforms is identical across two
//	      fetches (drift/sync rely on this)
//	perf  parallel export speed (target: 5000 objects < 10 min, §16)
//
// Everything here is read-only. (A proxy-auth check was dropped: the deploy
// account connects directly, see spec §10.3.)
//
// Passwords come from the environment, never from flags:
//
//	OVC_SPIKE_READER_PASSWORD    password of OVC_READER
//
// Example:
//
//	OVC_SPIKE_READER_PASSWORD=... go run ./cmd/spike -host dbdev -service DEVPDB -schema HR -check ddl
package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	go_ora "github.com/sijms/go-ora/v2"
)

var (
	host    = flag.String("host", "", "DB host")
	port    = flag.Int("port", 1521, "DB port")
	service = flag.String("service", "", "service name / PDB")
	schema  = flag.String("schema", "", "schema to inspect")
	reader  = flag.String("reader", "OVC_READER", "read-only user")
	checks  = flag.String("check", "ddl,perf", "comma list: ddl,perf")
	limit   = flag.Int("limit", 50, "objects to sample for ddl check")
	perfN   = flag.Int("perf-objects", 500, "objects to export in perf check")
	workers = flag.Int("workers", 8, "parallel connections for perf check")
)

func main() {
	flag.Parse()
	if *host == "" || *service == "" || *schema == "" {
		fmt.Fprintln(os.Stderr, "need -host, -service, -schema")
		os.Exit(2)
	}
	ctx := context.Background()
	failed := false
	for _, c := range strings.Split(*checks, ",") {
		var err error
		switch strings.TrimSpace(c) {
		case "ddl":
			err = checkDDL(ctx)
		case "perf":
			err = checkPerf(ctx)
		default:
			err = fmt.Errorf("unknown check %q", c)
		}
		if err != nil {
			failed = true
			fmt.Printf("FAIL %s: %v\n\n", c, err)
		} else {
			fmt.Printf("PASS %s\n\n", c)
		}
	}
	if failed {
		os.Exit(1)
	}
}

func open(user, passEnv string) (*sql.DB, error) {
	pass := os.Getenv(passEnv)
	if pass == "" {
		return nil, fmt.Errorf("env %s is empty", passEnv)
	}
	db, err := sql.Open("oracle", go_ora.BuildUrl(*host, *port, *service, user, pass, nil))
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect as %s: %w", user, err)
	}
	return db, nil
}

// ---- check 2/3: GET_DDL ----------------------------------------------------

type obj struct{ Type, Name string }

func listObjects(ctx context.Context, db *sql.DB, n int) ([]obj, error) {
	// OVC_READER sees everything through dba_objects; a plain read-only user
	// only sees what it may access through all_objects.
	out, err := listFrom(ctx, db, "dba_objects", n)
	if err != nil {
		fmt.Printf("note: dba_objects not readable (%v); falling back to all_objects\n", firstLine(err))
		return listFrom(ctx, db, "all_objects", n)
	}
	return out, nil
}

func firstLine(err error) string {
	return strings.SplitN(err.Error(), "\n", 2)[0]
}

func listFrom(ctx context.Context, db *sql.DB, view string, n int) ([]obj, error) {
	// Take objects round-robin across types so a small sample is not all one type.
	rows, err := db.QueryContext(ctx, `
		SELECT object_type, object_name FROM (
		  SELECT object_type, object_name,
		         ROW_NUMBER() OVER (PARTITION BY object_type ORDER BY object_name) rn
		    FROM `+view+`
		   WHERE owner = :1
		     AND object_type IN ('PACKAGE','PACKAGE BODY','PROCEDURE','FUNCTION','VIEW','TRIGGER','TYPE','TYPE BODY','TABLE','SEQUENCE','SYNONYM','MATERIALIZED VIEW')
		     AND generated = 'N'
		     AND object_name NOT LIKE 'SYS\_%' ESCAPE '\' AND object_name NOT LIKE 'BIN$%'
		     AND object_name NOT LIKE 'MLOG$\_%' ESCAPE '\' AND object_name NOT LIKE 'RUPD$\_%' ESCAPE '\'
		)
		ORDER BY rn, object_type, object_name
		FETCH FIRST :2 ROWS ONLY`, strings.ToUpper(*schema), n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []obj
	for rows.Next() {
		var o obj
		if err := rows.Scan(&o.Type, &o.Name); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func mdType(t string) string {
	switch t {
	case "PACKAGE":
		return "PACKAGE_SPEC"
	case "TYPE":
		return "TYPE_SPEC"
	}
	return strings.ReplaceAll(t, " ", "_")
}

// setTransforms applies spec §9.1 step 4 to this session.
func setTransforms(ctx context.Context, c *sql.Conn) error {
	_, err := c.ExecContext(ctx, `BEGIN
	  DBMS_METADATA.SET_TRANSFORM_PARAM(DBMS_METADATA.SESSION_TRANSFORM,'SEGMENT_ATTRIBUTES',FALSE);
	  DBMS_METADATA.SET_TRANSFORM_PARAM(DBMS_METADATA.SESSION_TRANSFORM,'STORAGE',FALSE);
	  DBMS_METADATA.SET_TRANSFORM_PARAM(DBMS_METADATA.SESSION_TRANSFORM,'TABLESPACE',FALSE);
	  DBMS_METADATA.SET_TRANSFORM_PARAM(DBMS_METADATA.SESSION_TRANSFORM,'EMIT_SCHEMA',FALSE);
	  DBMS_METADATA.SET_TRANSFORM_PARAM(DBMS_METADATA.SESSION_TRANSFORM,'SQLTERMINATOR',TRUE);
	  DBMS_METADATA.SET_TRANSFORM_PARAM(DBMS_METADATA.SESSION_TRANSFORM,'PRETTY',TRUE);
	  DBMS_METADATA.SET_TRANSFORM_PARAM(DBMS_METADATA.SESSION_TRANSFORM,'CONSTRAINTS_AS_ALTER',TRUE);
	  DBMS_METADATA.SET_TRANSFORM_PARAM(DBMS_METADATA.SESSION_TRANSFORM,'REF_CONSTRAINTS',FALSE);
	END;`)
	return err
}

func getDDL(ctx context.Context, c *sql.Conn, o obj) (string, error) {
	var ddl string
	err := c.QueryRowContext(ctx, `SELECT DBMS_METADATA.GET_DDL(:1,:2,:3) FROM dual`,
		mdType(o.Type), o.Name, strings.ToUpper(*schema)).Scan(&ddl)
	return ddl, err
}

func checkDDL(ctx context.Context) error {
	fmt.Println("== GET_DDL stability ==")
	db, err := open(*reader, "OVC_SPIKE_READER_PASSWORD")
	if err != nil {
		return err
	}
	defer db.Close()
	objs, err := listObjects(ctx, db, *limit)
	if err != nil {
		return fmt.Errorf("list objects (needs SELECT ANY DICTIONARY / SELECT_CATALOG_ROLE): %w", err)
	}
	if len(objs) == 0 {
		return fmt.Errorf("no objects found for schema %s", *schema)
	}
	c, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := setTransforms(ctx, c); err != nil {
		return fmt.Errorf("set transforms: %w", err)
	}

	unstable, errs, lineEndings, hasSchemaName := 0, 0, 0, 0
	counts := map[string]int{}
	for _, o := range objs {
		a, errA := getDDL(ctx, c, o)
		b, errB := getDDL(ctx, c, o)
		if errA != nil || errB != nil {
			errs++
			fmt.Printf("  ERR  %-18s %-30s %v %v\n", o.Type, o.Name, errA, errB)
			continue
		}
		counts[o.Type]++
		if sha256.Sum256([]byte(a)) != sha256.Sum256([]byte(b)) {
			unstable++
			fmt.Printf("  DIFF %-18s %-30s two fetches differ\n", o.Type, o.Name)
		}
		if strings.Contains(a, "\r") {
			lineEndings++
		}
		if strings.Contains(strings.ToUpper(a), strings.ToUpper(*schema)+".") {
			hasSchemaName++
			fmt.Printf("  NOTE %-18s %-30s DDL still contains \"%s.\"\n", o.Type, o.Name, *schema)
		}
	}
	fmt.Printf("sampled %d objects %v\n", len(objs), counts)
	fmt.Printf("errors=%d unstable=%d with_CR=%d with_schema_prefix=%d\n", errs, unstable, lineEndings, hasSchemaName)
	fmt.Println("NOTE: schema prefix / CR hits are not failures; they tell us what the normaliser (§9.1 step 5) must handle.")
	if errs > 0 || unstable > 0 {
		return fmt.Errorf("GET_DDL not reliable enough: %d errors, %d unstable", errs, unstable)
	}
	return nil
}

func checkPerf(ctx context.Context) error {
	fmt.Println("== parallel export throughput ==")
	db, err := open(*reader, "OVC_SPIKE_READER_PASSWORD")
	if err != nil {
		return err
	}
	objs, err := listObjects(ctx, db, *perfN)
	db.Close()
	if err != nil {
		return err
	}
	if len(objs) == 0 {
		return fmt.Errorf("no objects found for schema %s", *schema)
	}
	jobs := make(chan obj)
	var done, failed, bytes atomic.Int64
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < *workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wdb, err := open(*reader, "OVC_SPIKE_READER_PASSWORD")
			if err != nil {
				fmt.Println("  worker connect:", err)
				for range jobs {
					failed.Add(1)
				}
				return
			}
			defer wdb.Close()
			c, err := wdb.Conn(ctx)
			if err != nil {
				for range jobs {
					failed.Add(1)
				}
				return
			}
			defer c.Close()
			if err := setTransforms(ctx, c); err != nil {
				for range jobs {
					failed.Add(1)
				}
				return
			}
			for o := range jobs {
				ddl, err := getDDL(ctx, c, o)
				if err != nil {
					failed.Add(1)
					continue
				}
				bytes.Add(int64(len(ddl)))
				done.Add(1)
			}
		}()
	}
	for _, o := range objs {
		jobs <- o
	}
	close(jobs)
	wg.Wait()
	el := time.Since(start)
	rate := float64(done.Load()) / el.Seconds()
	fmt.Printf("%d ok, %d failed, %.1f KB in %s with %d workers = %.1f objects/s\n",
		done.Load(), failed.Load(), float64(bytes.Load())/1024, el.Round(time.Millisecond), *workers, rate)
	if rate > 0 {
		fmt.Printf("projection: 5000 objects ≈ %s (target < 10m)\n", (time.Duration(5000/rate) * time.Second).Round(time.Second))
	}
	if failed.Load() > 0 {
		return fmt.Errorf("%d objects failed", failed.Load())
	}
	return nil
}
