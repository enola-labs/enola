package flows

import (
	"path"
	"regexp"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// Calls that leave the repository.
//
// unresolved_calls counts every call the walk could not follow, and most of them are
// calls into the standard library and imported packages: the graph holds the
// repository's own symbols, so the walk ends at its edge by design. external_calls is
// the part of that count the snapshot can show leaves the repository: the call names
// a package its file imports from outside it, as the file's dependency facts record.
//
// The evidence is the import, so the count errs one way only. A call into a package
// imported under an alias the target does not spell stays unresolved, and is never
// counted as a library call when it was a gap.

// versionSuffix is the major-version element of a Go module path, `/v2`, which the
// package's name does not include.
var versionSuffix = regexp.MustCompile(`/v[0-9]+$`)

// externalImports returns, per file, the imports from outside the repository: the
// imported paths and the names a call can reach them by.
func (a *Analysis) externalImports() map[string][]string {
	if a.imports != nil {
		return a.imports
	}
	a.imports = map[string][]string{}
	for _, f := range a.src.FactsRef() {
		if f.Kind != facts.KindDependency || f.File == "" {
			continue
		}
		switch f.PropString("source") {
		case "", "internal", "relative", "local":
			continue
		}
		for _, r := range f.Relations {
			if r.Kind != facts.RelImports || r.Target == "" {
				continue
			}
			key := f.Repo + "\x00" + f.File
			a.imports[key] = append(a.imports[key], r.Target)
		}
	}
	return a.imports
}

// isExternalCall reports whether a call from a symbol of file names a package that
// file imports from outside the repository, written either as the imported path
// (`github.com/x/y.Func`, `@scope/pkg.fn`, `Illuminate\Support\Str::random`) or
// through the package's name (`json.Marshal` for encoding/json).
func (a *Analysis) isExternalCall(repo, file, target string) bool {
	for _, imp := range a.externalImports()[repo+"\x00"+file] {
		if rest, ok := strings.CutPrefix(target, imp); ok && rest != "" && strings.ContainsRune(".:\\#/", rune(rest[0])) {
			return true
		}
		name := path.Base(versionSuffix.ReplaceAllString(imp, ""))
		if head, _, ok := strings.Cut(target, "."); ok && head == name {
			return true
		}
	}
	return false
}
