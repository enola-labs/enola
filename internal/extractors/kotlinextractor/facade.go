package kotlinextractor

import (
	"path"
	"strings"
	"unicode"
)

// kotlinFacadeName is the facade class name Kotlin gives a file's top-level
// declarations: the file name without .kt, every character that cannot be part
// of a Java identifier replaced by `_`, the first letter upper-cased, then `Kt`
// (`main.ios.kt` is Main_iosKt).
func kotlinFacadeName(rel string) string {
	base := strings.TrimSuffix(path.Base(rel), path.Ext(rel))
	if base == "" {
		return "_Kt"
	}
	r := []rune(base)
	for i, c := range r {
		if !unicode.IsLetter(c) && !unicode.IsDigit(c) && c != '_' && c != '$' {
			r[i] = '_'
		}
	}
	if r[0] >= 'a' && r[0] <= 'z' {
		r[0] = r[0] - 'a' + 'A'
	}
	return string(r) + "Kt"
}
