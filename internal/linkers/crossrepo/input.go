package crossrepo

import (
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// input is the plugin.SignalInput handed to every signal: the fact set plus the derived
// indexes signals share.
//
// Every accessor here used to be a package-level function that walked the ENTIRE fact
// set on each call — and several were called more than once per link, by different
// signals that had no way to share the result. On a large multi-repo snapshot that is
// the same traversal repeated five or six times for indexes that never change. Here
// each is computed on first use and reused, so a signal that needs the same index costs
// nothing, and adding one does not add a walk.
//
// It is not safe for concurrent use: signals run sequentially within a phase, and the
// memoization is deliberately unsynchronized rather than paying for a mutex on a hot
// read path that has no concurrency to protect against.
type input struct {
	all []facts.Fact
	src SourceReader

	repos       []string          // sorted labels
	normToLabel map[string]string // normalized label -> actual

	primaryLang map[string]string
	topDirs     map[string]map[string]bool
	ownScopes   map[string]map[string]bool
	moduleNames map[string][]string

	// fileCache memoizes source reads so a file behind several shared identities is
	// read once per snapshot rather than once per comparison.
	fileCache map[string]string
	fileMiss  map[string]bool
}

func newInput(all []facts.Fact, src SourceReader) *input {
	in := &input{all: all, src: src, normToLabel: map[string]string{}}
	for _, f := range all {
		if f.Repo != "" {
			in.normToLabel[facts.NormalizeRepoLabel(f.Repo)] = f.Repo
		}
	}
	in.repos = make([]string, 0, len(in.normToLabel))
	for _, label := range in.normToLabel {
		in.repos = append(in.repos, label)
	}
	sort.Strings(in.repos)
	return in
}

func (in *input) Facts() []facts.Fact { return in.all }

func (in *input) Repos() []string { return in.repos }

func (in *input) ResolveRepo(candidate string) (string, bool) {
	label, ok := in.normToLabel[facts.NormalizeRepoLabel(candidate)]
	return label, ok
}

// PrimaryLanguage returns each repo's dominant source language (the most common
// `language` prop across its facts), or "" when unknown. It gates unqualified
// shared-symbol links to same-language repo pairs, so two apps in different languages
// sharing a plain domain type name are not linked.
func (in *input) PrimaryLanguage(repo string) string {
	if in.primaryLang == nil {
		counts := map[string]map[string]int{}
		for _, f := range in.all {
			if f.Repo == "" {
				continue
			}
			lang := f.PropString("language")
			if lang == "" {
				continue
			}
			if counts[f.Repo] == nil {
				counts[f.Repo] = map[string]int{}
			}
			counts[f.Repo][lang]++
		}
		in.primaryLang = map[string]string{}
		for r, langs := range counts {
			best, bestN := "", -1
			for l, n := range langs {
				if n > bestN || (n == bestN && l < best) {
					best, bestN = l, n
				}
			}
			in.primaryLang[r] = best
		}
	}
	return in.primaryLang[repo]
}

// TopDirs returns, per repo, the normalized leading segment of every module NAME the
// repo declares — the top-level source directories its own code lives under.
//
// It exists to stop a repo's own files from fabricating a cross-repo edge: an import
// target rooted at one of these is an intra-repo reference whose INTERIOR segments may
// coincide with another repo's label (an "app/src/main/java/de/backend/…" package path
// beside a backend repo labeled "backend"). Keyed on the module name rather than the
// file path because a module fact is the repo's own statement of its structure, and
// several extractors emit modules with no file at all.
func (in *input) TopDirs(repo string) map[string]bool {
	if in.topDirs == nil {
		in.topDirs = map[string]map[string]bool{}
		for _, f := range in.all {
			if f.Kind != facts.KindModule || f.Repo == "" {
				continue
			}
			seg := f.Name
			if i := strings.IndexByte(seg, '/'); i >= 0 {
				seg = seg[:i]
			}
			if seg == "" {
				continue
			}
			if in.topDirs[f.Repo] == nil {
				in.topDirs[f.Repo] = map[string]bool{}
			}
			in.topDirs[f.Repo][facts.NormalizeRepoLabel(seg)] = true
		}
	}
	return in.topDirs[repo]
}

// OwnScopes returns, per repo, the set of normalized npm @scopes it publishes under,
// read from the scoped module names it declares. A scope is a namespace, not a
// directory, so it never appears in TopDirs — yet "@acme/x" imported from the repo that
// publishes "@acme/y" is a sibling package of the same project, not a dependency on a
// repo that happens to be labeled "acme".
func (in *input) OwnScopes(repo string) map[string]bool {
	if in.ownScopes == nil {
		in.ownScopes = map[string]map[string]bool{}
		for _, f := range in.all {
			// Module facts only. A manifest dependency fact carries a package_name
			// too, naming the package the repository DEPENDS ON, and reading that as
			// published made every scoped dependency in package.json suppress the
			// import edge to the repository that publishes it: a service depending on
			// @org/sdk was taken to publish under @org itself.
			if f.Kind != facts.KindModule || f.Repo == "" {
				continue
			}
			// The scope lives on the module fact's package_name prop — the "name"
			// field of the package.json the module was read from — NOT on the fact's
			// Name, which is the directory path.
			name := f.PropString("package_name")
			if !strings.HasPrefix(name, "@") {
				continue
			}
			scope := name
			if i := strings.IndexByte(scope, '/'); i >= 0 {
				scope = scope[:i]
			}
			scope = strings.TrimPrefix(scope, "@")
			if scope == "" {
				continue
			}
			if in.ownScopes[f.Repo] == nil {
				in.ownScopes[f.Repo] = map[string]bool{}
			}
			in.ownScopes[f.Repo][facts.NormalizeRepoLabel(scope)] = true
		}
	}
	return in.ownScopes[repo]
}

// ModuleNames returns a repo's module names, longest first, so a type identity can have
// its module prefix stripped by the most specific match.
func (in *input) ModuleNames(repo string) []string {
	if in.moduleNames == nil {
		in.moduleNames = map[string][]string{}
		for _, f := range in.all {
			if f.Kind != facts.KindModule || f.Repo == "" {
				continue
			}
			in.moduleNames[f.Repo] = append(in.moduleNames[f.Repo], f.Name)
		}
		for r := range in.moduleNames {
			names := in.moduleNames[r]
			sort.Slice(names, func(i, j int) bool {
				if len(names[i]) != len(names[j]) {
					return len(names[i]) > len(names[j])
				}
				return names[i] < names[j]
			})
		}
	}
	return in.moduleNames[repo]
}

// ReadSource returns the text of the file a fact came from, memoized per snapshot.
// Reports false when no SourceReader was supplied or the file could not be read — which
// callers must treat as "cannot verify", never as "does not match".
func (in *input) HasSource() bool { return in.src != nil }

func (in *input) ReadSource(f facts.Fact) (string, bool) {
	if in.src == nil {
		return "", false
	}
	if in.fileCache == nil {
		in.fileCache = map[string]string{}
		in.fileMiss = map[string]bool{}
	}
	if text, ok := in.fileCache[f.File]; ok {
		return text, true
	}
	if in.fileMiss[f.File] {
		return "", false
	}
	text, ok := in.src(f)
	if !ok {
		in.fileMiss[f.File] = true
		return "", false
	}
	in.fileCache[f.File] = text
	return text, true
}
