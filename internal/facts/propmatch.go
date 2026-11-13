package facts

import (
	"fmt"
	"strconv"
	"strings"
)

// Prop comparison operators for QueryOpts.PropOp.
const (
	PropOpEq       = "eq"
	PropOpNe       = "ne"
	PropOpLt       = "lt"
	PropOpLe       = "le"
	PropOpGt       = "gt"
	PropOpGe       = "ge"
	PropOpPrefix   = "prefix"
	PropOpContains = "contains"
)

// PropOps lists the operators QueryOpts.PropOp accepts.
var PropOps = []string{PropOpEq, PropOpNe, PropOpLt, PropOpLe, PropOpGt, PropOpGe, PropOpPrefix, PropOpContains}

// ValidPropOp reports whether op is an operator PropMatches knows ("" is eq).
func ValidPropOp(op string) bool {
	if op == "" {
		return true
	}
	for _, o := range PropOps {
		if o == op {
			return true
		}
	}
	return false
}

// PropMatches reports whether a prop value satisfies op against want. A list
// matches when any element does (eq also matches the list's rendering, as before).
// Ordered operators compare as dotted versions when both sides read as one
// (0.138.0 < 0.150, v1.9 < v1.10), as numbers when both read as one, else as
// strings.
func PropMatches(v any, op, want string) bool {
	if op == "" {
		op = PropOpEq
	}
	switch l := v.(type) {
	case []string:
		if op == PropOpEq && fmt.Sprint(v) == want {
			return true
		}
		for _, e := range l {
			if scalarMatches(e, op, want) {
				return true
			}
		}
		return false
	case []any:
		if op == PropOpEq && fmt.Sprint(v) == want {
			return true
		}
		for _, e := range l {
			if scalarMatches(fmt.Sprint(e), op, want) {
				return true
			}
		}
		return false
	}
	return scalarMatches(fmt.Sprint(v), op, want)
}

func scalarMatches(got, op, want string) bool {
	switch op {
	case PropOpEq:
		return got == want
	case PropOpNe:
		return got != want
	case PropOpPrefix:
		return strings.HasPrefix(got, want)
	case PropOpContains:
		return strings.Contains(got, want)
	}
	c := compareValues(got, want)
	switch op {
	case PropOpLt:
		return c < 0
	case PropOpLe:
		return c <= 0
	case PropOpGt:
		return c > 0
	case PropOpGe:
		return c >= 0
	}
	return false
}

// compareValues orders two values as versions, else numbers, else strings.
func compareValues(a, b string) int {
	if va, ok := version(a); ok {
		if vb, ok := version(b); ok {
			for i := 0; i < len(va) || i < len(vb); i++ {
				x, y := 0, 0
				if i < len(va) {
					x = va[i]
				}
				if i < len(vb) {
					y = vb[i]
				}
				if x != y {
					if x < y {
						return -1
					}
					return 1
				}
			}
			return 0
		}
	}
	if fa, err := strconv.ParseFloat(a, 64); err == nil {
		if fb, err := strconv.ParseFloat(b, 64); err == nil {
			switch {
			case fa < fb:
				return -1
			case fa > fb:
				return 1
			}
			return 0
		}
	}
	return strings.Compare(a, b)
}

// version reads a dotted version of at least two numeric parts, an optional
// leading v, and a pre-release or build suffix ignored: 1.2.3-rc1 is [1 2 3].
func version(s string) ([]int, bool) {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "v"), "V")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return nil, false
	}
	out := make([]int, len(parts))
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, false
		}
		out[i] = n
	}
	return out, true
}
