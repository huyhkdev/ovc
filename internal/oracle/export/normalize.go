package export

import (
	"regexp"
	"strings"

	"ovc/internal/layout"
)

var (
	editionableRe = regexp.MustCompile(`(?m)^(CREATE OR REPLACE (?:FORCE )?)EDITIONABLE `)
	trailingSemi  = regexp.MustCompile(`\s+;\s*$`)
	startWithRe   = regexp.MustCompile(`\s+START WITH\s+\d+`)
)

// Normalize makes DBMS_METADATA output stable and portable (spec §5.2, §9.1
// step 5): LF line endings, no trailing blanks, no leading indent/blank lines,
// a single final newline, and no environment-dependent noise:
//
//   - EDITIONABLE after CREATE OR REPLACE is dropped (it is the default);
//   - a sequence's START WITH is dropped: GET_DDL writes the current value
//     there, so it would change on every NEXTVAL and look like drift.
//
// Normalize is idempotent.
func Normalize(t layout.ObjectType, ddl string) string {
	s := strings.ReplaceAll(ddl, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	s = strings.TrimSpace(strings.Join(lines, "\n"))
	s = editionableRe.ReplaceAllString(s, "$1")
	s = trailingSemi.ReplaceAllString(s, ";")
	if t == layout.Sequence {
		s = startWithRe.ReplaceAllString(s, "")
	}
	return s + "\n"
}
