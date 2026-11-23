package kotlinextractor

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/enola-labs/enola/internal/extractors/jvmsrc"
	"github.com/enola-labs/enola/internal/parallel"
	sitter "github.com/enola-labs/enola/internal/treesitter"
)

// A test's annotations are mostly the harness, but an annotation class of the
// repository applied only by tests (a DI qualifier, a test-scope marker) is used
// by them, and without the reference it reads as dead rather than test-only. The
// test-ref pass has no production index, so an annotation is resolved the way the
// compiler would, against the file's imports, its own package and its star
// imports, and recorded only when the result is a type production code declares.

// kotlinProdTypes is the set of top-level names production JVM sources declare, by
// fully qualified name: "<package>.<Name>".
func kotlinProdTypes(ctx context.Context, repoPath string, prodFiles []string) map[string]bool {
	var jvmFiles []string
	for _, relFile := range prodFiles {
		if l := strings.ToLower(relFile); strings.HasSuffix(l, ".kt") || strings.HasSuffix(l, ".java") {
			jvmFiles = append(jvmFiles, relFile)
		}
	}
	perFile := parallel.MapFiles(ctx, jvmFiles, func(relFile string) []string {
		src, err := os.ReadFile(filepath.Join(repoPath, relFile))
		if err != nil {
			return nil
		}
		pkg, names := jvmsrc.TopLevelNames(src)
		for i, n := range names {
			names[i] = qualifyKotlin(pkg, n)
		}
		return names
	})
	out := make(map[string]bool)
	for _, names := range perFile {
		for _, n := range names {
			out[n] = true
		}
	}
	return out
}

func qualifyKotlin(pkg, name string) string {
	if pkg == "" {
		return name
	}
	return pkg + "." + name
}

// collectHeader reads the package and the imports an annotation name resolves
// against: `import a.B` and `import a.B as C` bind a simple name, `import a.*`
// opens a package.
func (c *kotlinTestRefCollector) collectHeader(root *sitter.Node) {
	for i := uint(0); i < root.NamedChildCount(); i++ {
		n := root.NamedChild(i)
		switch kindOf(n) {
		case "package_header":
			if q := findChildByKind(n, "qualified_identifier"); q != nil {
				c.pkg = nodeText(q, c.src)
			}
		case "import":
			q := findChildByKind(n, "qualified_identifier")
			if q == nil {
				continue
			}
			path := nodeText(q, c.src)
			if strings.HasSuffix(strings.TrimSpace(nodeText(n, c.src)), "*") {
				c.wildcards = append(c.wildcards, path)
				continue
			}
			simple := path[strings.LastIndexByte(path, '.')+1:]
			if alias := findChildByKind(n, "identifier"); alias != nil {
				simple = nodeText(alias, c.src)
			}
			c.imports[simple] = path
		}
	}
}

// addAnnotation records an annotation the test applies when its name resolves to
// a production type of the repository, or a type nested in one. The recorded
// target is the type's simple name, the shape of every other type reference this
// pass emits.
func (c *kotlinTestRefCollector) addAnnotation(ann *sitter.Node) {
	ut := annotationType(ann)
	if ut == nil || len(c.prodTypes) == 0 {
		return
	}
	var segs []string
	for i := uint(0); i < ut.NamedChildCount(); i++ {
		if ch := ut.NamedChild(i); kindOf(ch) == "identifier" {
			segs = append(segs, nodeText(ch, c.src))
		}
	}
	for _, cand := range c.annotationCandidates(segs) {
		if !c.prodTypes[cand] && !c.prodTypes[enclosingType(cand)] {
			continue
		}
		if simple := cand[strings.LastIndexByte(cand, '.')+1:]; !c.declared[simple] {
			c.seen[simple] = true
		}
		return
	}
}

// annotationCandidates are the fully qualified names an annotation path may
// denote, in the order Kotlin resolves it: an explicit import, the file's own
// package, then its star imports. A path starting lower-case is already qualified.
// A name Kotlin imports by default resolves to nothing unless the file imports it,
// as in production code (defaultAnnotation).
func (c *kotlinTestRefCollector) annotationCandidates(segs []string) []string {
	if len(segs) == 0 {
		return nil
	}
	head, rest := segs[0], strings.Join(segs[1:], ".")
	if rest != "" {
		rest = "." + rest
		if h := head[0]; h >= 'a' && h <= 'z' {
			return []string{head + rest}
		}
	}
	if fqn, ok := c.imports[head]; ok {
		return []string{fqn + rest}
	}
	if rest == "" && kotlinDefaultAnnotations[head] {
		return nil
	}
	out := []string{qualifyKotlin(c.pkg, head) + rest}
	for _, wc := range c.wildcards {
		out = append(out, wc+"."+head+rest)
	}
	return out
}

// enclosingType is the type a nested name is declared in: "a.Outer.Inner" is
// "a.Outer". It is "" when the qualifier is a package (lower-case, by convention),
// so a name nothing declares is never credited to a top-level function or value
// that shares its package's last segment.
func enclosingType(name string) string {
	i := strings.LastIndexByte(name, '.')
	if i < 0 {
		return ""
	}
	parent := name[:i]
	last := parent[strings.LastIndexByte(parent, '.')+1:]
	if last == "" || last[0] < 'A' || last[0] > 'Z' {
		return ""
	}
	return parent
}
