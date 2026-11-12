package helmextractor

import (
	"strconv"
	"strings"
)

// satisfies reports whether version meets a Helm dependency constraint. It reads
// the forms charts write: an exact version, x and * wildcards (2.x.x), ^ and ~,
// comparison operators, comma- or space-separated conjunctions and || alternatives.
// Fail closed: a constraint or version it cannot read is not satisfied, so an
// in-repo chart is never matched to a dependency on the strength of a guess. An
// empty constraint accepts any version, as Helm does.
func satisfies(version, constraint string) bool {
	constraint = strings.TrimSpace(constraint)
	if constraint == "" || constraint == "*" {
		return true
	}
	v, ok := parseVersion(version)
	if !ok {
		return false
	}
	for _, alt := range strings.Split(constraint, "||") {
		if allOf(v, alt) {
			return true
		}
	}
	return false
}

func allOf(v [3]int, conj string) bool {
	fields := strings.FieldsFunc(conj, func(r rune) bool { return r == ',' || r == ' ' })
	if len(fields) == 0 {
		return false
	}
	// "a - b" is a hyphen range.
	if len(fields) == 3 && fields[1] == "-" {
		return compare(v, ">="+fields[0]) && compare(v, "<="+fields[2])
	}
	for _, f := range fields {
		if !compare(v, f) {
			return false
		}
	}
	return true
}

func compare(v [3]int, c string) bool {
	op := ""
	for _, p := range []string{">=", "<=", "!=", ">", "<", "=", "^", "~"} {
		if strings.HasPrefix(c, p) {
			op, c = p, strings.TrimSpace(c[len(p):])
			break
		}
	}
	want, wild, ok := parsePattern(c)
	if !ok {
		return false
	}
	switch op {
	case "", "=":
		for i := 0; i < 3; i++ {
			if !wild[i] && v[i] != want[i] {
				return false
			}
			if wild[i] {
				return true
			}
		}
		return true
	case "!=":
		return cmp(v, want) != 0
	case ">=":
		return cmp(v, want) >= 0
	case "<=":
		return cmp(v, want) <= 0
	case ">":
		return cmp(v, want) > 0
	case "<":
		return cmp(v, want) < 0
	case "^":
		if cmp(v, want) < 0 {
			return false
		}
		switch {
		case want[0] > 0:
			return v[0] == want[0]
		case want[1] > 0:
			return v[0] == 0 && v[1] == want[1]
		default:
			return v[0] == 0 && v[1] == 0 && v[2] == want[2]
		}
	case "~":
		return cmp(v, want) >= 0 && v[0] == want[0] && (wild[1] || v[1] == want[1])
	}
	return false
}

func cmp(a, b [3]int) int {
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// parseVersion reads major.minor.patch, ignoring a leading v and any pre-release or
// build suffix.
func parseVersion(s string) ([3]int, bool) {
	v, wild, ok := parsePattern(s)
	if !ok || wild[0] || wild[1] || wild[2] {
		return v, false
	}
	return v, true
}

func parsePattern(s string) ([3]int, [3]bool, bool) {
	var v [3]int
	var wild [3]bool
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return v, wild, false
	}
	parts := strings.Split(s, ".")
	if len(parts) > 3 {
		return v, wild, false
	}
	for i := 0; i < 3; i++ {
		if i >= len(parts) {
			wild[i] = true
			continue
		}
		p := parts[i]
		if p == "x" || p == "X" || p == "*" {
			wild[i] = true
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return v, wild, false
		}
		v[i] = n
	}
	return v, wild, true
}
