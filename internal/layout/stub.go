package layout

import (
	"fmt"
	"strings"
)

// stubPrefix starts the single line of a stub file (spec §5.5).
const stubPrefix = "-- OVC:STUB"

// StubInfo is the metadata carried by a stub file.
type StubInfo struct {
	Type ObjectType
}

// RenderStub returns the content of a stub file:
//
//	-- OVC:STUB type=PACKAGE_BODY
//
// It carries nothing that differs between databases, so the stub of an object
// is byte-identical on every <schema>_<env> branch and merging between them
// never conflicts on stubs. The init-time last_ddl_time lives on the server.
func RenderStub(t ObjectType) string {
	return fmt.Sprintf("%s type=%s\n", stubPrefix, strings.ReplaceAll(string(t), " ", "_"))
}

// IsStub reports whether content is a stub (first non-empty line starts with
// the stub marker). Real DDL must never start with it.
func IsStub(content []byte) bool {
	s := strings.TrimLeft(string(content), " \t\r\n")
	return strings.HasPrefix(s, stubPrefix)
}

// ParseStub reads the metadata of a stub file.
func ParseStub(content string) (StubInfo, error) {
	line := strings.TrimLeft(content, " \t\r\n")
	if !strings.HasPrefix(line, stubPrefix) {
		return StubInfo{}, fmt.Errorf("layout: not a stub")
	}
	line, _, _ = strings.Cut(line, "\n")
	var info StubInfo
	for _, f := range strings.Fields(strings.TrimPrefix(line, stubPrefix)) {
		k, v, ok := strings.Cut(f, "=")
		if !ok || k != "type" {
			continue // unknown keys are ignored
		}
		t := ObjectType(strings.ReplaceAll(v, "_", " "))
		if _, known := specs[t]; !known {
			return StubInfo{}, fmt.Errorf("layout: stub has unknown type %q", v)
		}
		info.Type = t
	}
	if info.Type == "" {
		return StubInfo{}, fmt.Errorf("layout: stub has no type")
	}
	return info, nil
}
