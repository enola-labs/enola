package constraints

import (
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/intent"
)

// goNames lets a declaration written before Go packages were named by import path
// keep meaning what it meant. A name pattern, a to_name and an exemption's witness
// all spell Go names, and those written against a directory-named package
// ("internal/server.*", "pkg/a -> example.com/app/pkg/b -> pkg/b via imports") would
// otherwise match nothing after the rename, so an excused breach would fail a build
// on an upgrade that changed nothing in the code. Each is matched as written and in
// the directory form; a declaration written with import paths needs neither.
type goNames struct {
	// dirOf maps a Go package's import-path name to its repo-relative directory,
	// "." for a module root.
	dirOf map[string]string
	// byDir maps a directory to the packages living there, one per repository.
	byDir map[string][]string
}

func newGoNames(store *facts.Store) goNames {
	g := goNames{dirOf: map[string]string{}, byDir: map[string][]string{}}
	for _, m := range store.ByKind(facts.KindModule) {
		if !facts.IsGoPackage(m) || m.File == "" || m.File == m.Name {
			continue
		}
		dir := m.File
		if m.Repo != "" {
			dir = strings.TrimPrefix(dir, m.Repo+"/")
		}
		g.dirOf[m.Name] = dir
		g.byDir[dir] = append(g.byDir[dir], m.Name)
	}
	return g
}

// legacy returns name with its Go package written as its directory, the way names
// read before packages were named by import path ("example.com/app/internal/server.
// Server" is "internal/server.Server", "example.com/app.main" is "..main"), or "" for
// a name that is not under a renamed package.
func (g goNames) legacy(name string) string {
	if len(g.dirOf) == 0 {
		return ""
	}
	if d, ok := g.dirOf[name]; ok {
		return d
	}
	for i := len(name) - 1; i > 0; i-- {
		if name[i] == '.' {
			if d, ok := g.dirOf[name[:i]]; ok {
				return d + name[i:]
			}
		}
	}
	return ""
}

// matchName reports whether name matches pattern as written or in its directory form.
func (g goNames) matchName(name, pattern string) bool {
	if intent.MatchBoundedName(name, pattern) {
		return true
	}
	l := g.legacy(name)
	return l != "" && intent.MatchBoundedName(l, pattern)
}

// upgradeWitness rewrites a witness's two ends, "<from> -> … -> <to> via <kind>",
// from a Go package's directory to its import-path name, which is how witnesses read
// now; "" when neither end is such a directory, or names a directory several
// repositories share, where the old spelling cannot say which was meant.
func (g goNames) upgradeWitness(w string) string {
	if len(g.byDir) == 0 {
		return ""
	}
	parts := strings.Split(w, " -> ")
	if len(parts) < 2 {
		return ""
	}
	last, via, _ := strings.Cut(parts[len(parts)-1], " via ")
	up := func(tok string) (string, bool) {
		if names := g.byDir[tok]; len(names) == 1 {
			return names[0], true
		}
		return tok, false
	}
	first, a := up(parts[0])
	last, b := up(last)
	if !a && !b {
		return ""
	}
	parts[0] = first
	if via != "" {
		last += " via " + via
	}
	parts[len(parts)-1] = last
	return strings.Join(parts, " -> ")
}
