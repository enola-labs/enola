package kotlinextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// Compile-time dependency injection containers (Metro, Anvil).
//
//	@BindingContainer
//	@ContributesTo(AppScope::class)
//	object AndroidBindings { @Provides fun context(app: Application): Context = app }
//
// The compiler merges a container contributed to a scope into that scope's graph;
// nothing names it in code, as Hilt installs an @InstallIn module. A container
// with no @ContributesTo is listed in a graph's bindingContainers, a class literal
// and so a reference, and stays reportable when no graph lists it.
//
// The annotations count only when imported from the framework that defines them,
// so an unrelated @ContributesTo means nothing here.

// diAnnotationPackages are the packages declaring the annotations read here.
var diAnnotationPackages = []string{"dev.zacsweers.metro", "com.squareup.anvil.annotations"}

// noteDIImport records an import of a DI framework annotation (or a wildcard
// import of its package) by the simple name it is used under.
func (w *astWalker) noteDIImport(importPath string, wildcard bool) {
	for _, pkg := range diAnnotationPackages {
		if wildcard && importPath == pkg {
			if w.diImports == nil {
				w.diImports = map[string]bool{}
			}
			w.diImports["*"] = true
			return
		}
		if simple, ok := strings.CutPrefix(importPath, pkg+"."); ok && !strings.Contains(simple, ".") {
			if w.diImports == nil {
				w.diImports = map[string]bool{}
			}
			w.diImports[simple] = true
			return
		}
	}
}

// diAnnotation reports whether a declaration carries a DI framework annotation.
func (w *astWalker) diAnnotation(annotations []string, name string) bool {
	return containsAnnotation(annotations, name) && (w.diImports[name] || w.diImports["*"])
}

// addDIContainerProps marks a Metro or Anvil container: a @BindingContainer is a
// DI module, and one contributed to a scope (@ContributesTo) is registered with
// the framework.
func (w *astWalker) addDIContainerProps(f *facts.Fact, annotations []string) {
	if w.diAnnotation(annotations, "BindingContainer") {
		f.SetProp("di_module", true)
	}
	if w.diAnnotation(annotations, "ContributesTo") {
		f.SetProp("framework_registered", true)
	}
}
