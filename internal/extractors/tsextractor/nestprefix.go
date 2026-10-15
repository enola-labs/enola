package tsextractor

import (
	"regexp"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

var (
	globalPrefixCall    = regexp.MustCompile(`\.setGlobalPrefix\s*\(`)
	globalPrefixLiteral = regexp.MustCompile("\\.setGlobalPrefix\\s*\\(\\s*(?:'([^'\\\\$]*)'|\"([^\"\\\\]*)\"|`([^`$\\\\]*)`)\\s*\\)")
)

func scanGlobalPrefixes(src []byte) []string {
	calls := globalPrefixCall.FindAllIndex(src, -1)
	if len(calls) == 0 {
		return nil
	}
	literal := make(map[int]string)
	for _, m := range globalPrefixLiteral.FindAllSubmatchIndex(src, -1) {
		for g := 1; g <= 3; g++ {
			if m[2*g] >= 0 {
				literal[m[0]] = string(src[m[2*g]:m[2*g+1]])
			}
		}
	}
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, literal[c[0]])
	}
	return out
}

func applyNestGlobalPrefix(all []facts.Fact, calls []string) string {
	if len(calls) == 0 {
		return ""
	}
	prefix := strings.Trim(calls[0], "/")
	for _, c := range calls {
		if c == "" || strings.Trim(c, "/") != prefix {
			return ""
		}
	}
	if prefix == "" {
		return ""
	}
	for i := range all {
		f := &all[i]
		if f.Kind != facts.KindRoute || f.PropString("framework") != "nestjs" ||
			f.PropString(facts.PropRole) != facts.RoleServer {
			continue
		}
		f.Name = facts.JoinRoutePath("/"+prefix, f.Name)
	}
	return prefix
}
