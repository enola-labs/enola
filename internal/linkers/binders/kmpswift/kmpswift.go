// Package kmpswift binds a Swift call of a Kotlin Multiplatform function to the
// Kotlin declaration.
//
// Kotlin/Native exports the top-level functions of a framework's Apple source
// sets to Swift as static members of each file's facade class: a Swift file that
// imports `shared` calls `Main_iosKt.MainViewController()` for the
// `fun MainViewController()` of main.ios.kt. Neither extractor sees the other
// language, so the Kotlin one stamps each exported function with its framework
// and facade (facts.PropKMPFramework, facts.PropKotlinFacade), and the Swift one
// lists the `Facade.member` calls a symbol makes (facts.PropKotlinFacadeCalls).
//
// A call binds only through a framework the calling Swift file imports, to a
// function of that framework whose facade is the receiver and whose exported
// name is the member, and only when exactly one function qualifies. A Swift type
// named like a facade, or a framework the file does not import, binds nothing.
package kmpswift

import (
	"context"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/plugin"
)

// Binder implements plugin.Binder.
type Binder struct{}

// New returns the binder.
func New() *Binder { return &Binder{} }

func (b *Binder) Name() string { return "kmp-swift" }

// Stage is post-link: it resolves references within a repository.
func (b *Binder) Stage() plugin.BindStage { return plugin.StagePostLink }

// export is one Kotlin function as Swift sees it.
type export struct {
	repo, framework, facade, member string
}

type fileKey struct{ repo, file string }

func (b *Binder) Bind(_ context.Context, store *facts.Store) error {
	all := store.FactsRef()
	exports := map[export][]string{}
	imports := map[fileKey]map[string]bool{}
	callers := false
	for i := range all {
		f := &all[i]
		switch f.Kind {
		case facts.KindSymbol:
			if len(propStrings(f.PropAny(facts.PropKotlinFacadeCalls))) > 0 {
				callers = true
			}
			facade := f.PropString(facts.PropKotlinFacade)
			fw := f.PropString(facts.PropKMPFramework)
			if facade == "" || fw == "" {
				continue
			}
			member := objcExportName(f.Name[strings.LastIndexByte(f.Name, '.')+1:])
			for _, name := range strings.Split(fw, ",") {
				k := export{f.Repo, name, facade, member}
				exports[k] = append(exports[k], f.Name)
			}
		case facts.KindDependency:
			if f.PropString("language") != "swift" {
				continue
			}
			for _, r := range f.Relations {
				if r.Kind == facts.RelImports {
					k := fileKey{f.Repo, f.File}
					if imports[k] == nil {
						imports[k] = map[string]bool{}
					}
					imports[k][r.Target] = true
				}
			}
		}
	}
	if !callers || len(exports) == 0 {
		return nil
	}
	store.UpdateWhere(func(f *facts.Fact) {
		if f.Kind != facts.KindSymbol {
			return
		}
		calls := propStrings(f.PropAny(facts.PropKotlinFacadeCalls))
		if len(calls) == 0 {
			return
		}
		frameworks := make([]string, 0, len(imports[fileKey{f.Repo, f.File}]))
		for fw := range imports[fileKey{f.Repo, f.File}] {
			frameworks = append(frameworks, fw)
		}
		sort.Strings(frameworks)
		for _, c := range calls {
			dot := strings.IndexByte(c, '.')
			if dot <= 0 {
				continue
			}
			facade, member := c[:dot], c[dot+1:]
			targets := map[string]bool{}
			for _, fw := range frameworks {
				for _, t := range exports[export{f.Repo, fw, facade, member}] {
					targets[t] = true
				}
			}
			if len(targets) != 1 {
				continue // none, or ambiguous
			}
			for t := range targets {
				if !f.HasRelation(facts.RelCalls, t) {
					f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelCalls, Target: t})
				}
			}
		}
	})
	return nil
}

func propStrings(v any) []string {
	switch p := v.(type) {
	case []string:
		return p
	case []any:
		out := make([]string, 0, len(p))
		for _, s := range p {
			if str, ok := s.(string); ok {
				out = append(out, str)
			}
		}
		return out
	}
	return nil
}

// objcExportName is the name Kotlin/Native gives a function in Objective-C and
// Swift: a name in the alloc, copy, mutableCopy, new or init method family (the
// family word, then the end or a non-lowercase letter, after any leading
// underscores) is prefixed with `do`, so `initApp` is `doInitApp`.
func objcExportName(name string) string {
	trimmed := strings.TrimLeft(name, "_")
	for _, family := range []string{"alloc", "copy", "mutableCopy", "new", "init"} {
		if !strings.HasPrefix(trimmed, family) {
			continue
		}
		rest := trimmed[len(family):]
		if rest == "" || rest[0] < 'a' || rest[0] > 'z' {
			return "do" + strings.ToUpper(name[:1]) + name[1:]
		}
	}
	return name
}
