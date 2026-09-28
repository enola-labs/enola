package tsextractor

import (
	"path"
	"strings"
)

// tsFileIndex finds the repository file an import specifier names when the
// specifier does not say so itself: a framework alias (`$lib/…` in SvelteKit,
// `~/…`, `@/…`, `#…`) or a baseUrl-rooted path (`app/_utils`). resolveImportPath
// classifies those as packages because only tsconfig `paths` tell it otherwise.
//
// A specifier is matched by suffix against the known files, so it names a file only
// when exactly one of them ends in it. Built once per run; read-only afterwards.
type tsFileIndex struct {
	byBase map[string][]string // last path segment -> file stems (paths without extension) ending in it
	stems  map[string]string   // file stem -> file path
}

func newTSFileIndex(knownFiles map[string]bool) *tsFileIndex {
	ix := &tsFileIndex{byBase: map[string][]string{}, stems: map[string]string{}}
	for file := range knownFiles {
		stem := strings.TrimSuffix(file, path.Ext(file))
		ix.stems[stem] = file
		ix.byBase[path.Base(stem)] = append(ix.byBase[path.Base(stem)], stem)
		if path.Base(stem) == "index" {
			// `import … from "x/y"` names y/index.ts as much as y.ts.
			ix.byBase[path.Base(path.Dir(stem))] = append(ix.byBase[path.Base(path.Dir(stem))], path.Dir(stem))
		}
	}
	return ix
}

// resolve returns the repository file the specifier, imported from fromFile, names,
// or false when it names none or cannot tell several apart. A framework alias's first
// segment stands for a directory the file path spells differently (`$lib` is
// `src/lib`), so it is dropped before matching. An unprefixed specifier must have two
// segments: one bare word is how every npm package is imported, and a local file
// sharing that word is no evidence.
//
// Several matches are told apart by nearness: an alias or a baseUrl is rooted in the
// importing project, so the file sharing the longest directory prefix with fromFile
// is the one it names (`$lib/utils` from web/src/lib/x.ts is web/src/lib/utils.ts,
// not server/src/utils.ts). A tie is no answer.
func (ix *tsFileIndex) resolve(spec, fromFile string) (string, bool) {
	if ix == nil || spec == "" {
		return "", false
	}
	rest := spec
	switch {
	case strings.HasPrefix(spec, "$"), strings.HasPrefix(spec, "~"), strings.HasPrefix(spec, "#"), strings.HasPrefix(spec, "@/"):
		i := strings.IndexByte(spec, '/')
		if i < 0 {
			return "", false
		}
		rest = spec[i+1:]
	case strings.HasPrefix(spec, "@"), !strings.Contains(spec, "/"):
		return "", false // a scoped or bare package name
	}
	rest = strings.TrimSuffix(rest, path.Ext(rest))
	if rest == "" {
		return "", false
	}
	found, best, tied := "", -1, false
	for _, stem := range ix.byBase[path.Base(rest)] {
		if stem != rest && !strings.HasSuffix(stem, "/"+rest) {
			continue
		}
		file, ok := ix.stems[stem]
		if !ok {
			file, ok = ix.stems[stem+"/index"]
		}
		if !ok || file == found {
			continue
		}
		switch near := sharedDirs(file, fromFile); {
		case near > best:
			found, best, tied = file, near, false
		case near == best:
			tied = true
		}
	}
	if tied {
		return "", false
	}
	return found, found != ""
}

// sharedDirs counts the leading directories two repository paths have in common.
func sharedDirs(a, b string) int {
	da, db := strings.Split(path.Dir(a), "/"), strings.Split(path.Dir(b), "/")
	n := 0
	for n < len(da) && n < len(db) && da[n] == db[n] {
		n++
	}
	return n
}
