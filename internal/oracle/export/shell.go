package export

import (
	"fmt"
	"strings"
	"time"

	"ovc/internal/layout"
)

// Shell is the set of stub files `ovc init` commits (spec §5.5, §9.1).
type Shell struct {
	Files    map[string]string    // repo-relative path -> stub content
	DDLTimes map[string]time.Time // repo-relative path -> last_ddl_time (kept by the server, not in the file)
	Warnings []string             // objects that could not be represented
}

// BuildShell turns an object list into stub files. Objects whose names cannot
// be a file name, or that collide on a case-insensitive file system, are
// skipped and reported in Warnings instead of failing the whole init.
func BuildShell(objs []Object) Shell {
	sh := Shell{Files: make(map[string]string, len(objs)), DDLTimes: make(map[string]time.Time, len(objs))}
	seen := map[string]string{} // lower-case path -> original path
	for _, o := range objs {
		p, err := layout.PathFor(o.Type, o.Name)
		if err != nil {
			sh.Warnings = append(sh.Warnings, fmt.Sprintf("skip %s %q: %v", o.Type, o.Name, err))
			continue
		}
		if prev, dup := seen[strings.ToLower(p)]; dup {
			sh.Warnings = append(sh.Warnings, fmt.Sprintf("skip %s %q: path %s collides with %s on case-insensitive file systems", o.Type, o.Name, p, prev))
			continue
		}
		seen[strings.ToLower(p)] = p
		sh.Files[p] = layout.RenderStub(o.Type)
		sh.DDLTimes[p] = o.LastDDLTime
	}
	return sh
}
