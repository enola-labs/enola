package rubyextractor

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	sitter "github.com/enola-labs/enola/internal/treesitter"
	ruby "github.com/tree-sitter/tree-sitter-ruby/bindings/go"
)

// TestCorpusParseCoverage measures what the vendored Ruby grammar can actually read on
// real repositories, and where its failures land. It is skipped by default; run it with
//
//	RUBY_CORPUS=$HOME/development/ror go test ./internal/extractors/rubyextractor/ \
//	    -run TestCorpusParseCoverage -v -timeout 30m
//
// Two columns, and only the second one matters much:
//
//   - "files with any error" is a ceiling, not a cost. An ERROR node inside a method body
//     loses that method's call edges and nothing else.
//   - "files losing a type" is the real number. When an error's nearest enclosing
//     definition is the compilation unit, the class or module around it never parsed — so
//     the type node is gone and its methods scatter to file scope. That is the Swift
//     flattening failure, and the Scala and Dart corpora were both measured this way
//     BEFORE an extractor was written rather than after.
//
// This had never been run for Ruby. Every route and association fix in this package rests
// on the grammar reading the file at all, so the number belongs on the record first.
func TestCorpusParseCoverage(t *testing.T) {
	root := os.Getenv("RUBY_CORPUS")
	if root == "" {
		t.Skip("corpus probe disabled; set RUBY_CORPUS=<dir of sibling Ruby repos> to run")
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("reading corpus dir %s: %v", root, err)
	}

	type result struct {
		repo                       string
		files, withError, typeLoss int
		worstDirs                  map[string]int
	}
	var results []result

	for _, ent := range entries {
		if !ent.IsDir() || strings.HasPrefix(ent.Name(), ".") {
			continue
		}
		repoPath := filepath.Join(root, ent.Name())
		res := result{repo: ent.Name(), worstDirs: map[string]int{}}

		parser := sitter.NewParser()
		if err := parser.SetLanguage(sitter.NewLanguage(ruby.Language())); err != nil {
			parser.Close()
			t.Fatalf("SetLanguage: %v", err)
		}

		_ = filepath.WalkDir(repoPath, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				switch d.Name() {
				case ".git", "vendor", "node_modules", "tmp", "log", ".enola":
					return filepath.SkipDir
				}
				return nil
			}
			if !isRubyFile(d.Name()) {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil || len(src) == 0 {
				return nil
			}
			res.files++

			tree := parser.Parse(src, nil)
			if tree == nil {
				return nil
			}
			defer tree.Close()
			rootNode := tree.RootNode()
			if !rootNode.HasError() {
				return nil
			}
			res.withError++
			if losesType(rootNode) {
				res.typeLoss++
				rel, _ := filepath.Rel(repoPath, path)
				res.worstDirs[topDirs(rel, 2)]++
			}
			return nil
		})
		parser.Close()

		if res.files > 0 {
			results = append(results, res)
		}
	}

	sort.Slice(results, func(i, j int) bool {
		return pct(results[i].typeLoss, results[i].files) < pct(results[j].typeLoss, results[j].files)
	})

	var b strings.Builder
	fmt.Fprintf(&b, "\n| Repo | Files | Files with any error | **Files losing a type** |\n")
	fmt.Fprintf(&b, "|---|---:|---:|---:|\n")
	var tf, te, tl int
	for _, r := range results {
		fmt.Fprintf(&b, "| %s | %d | %.2f%% | **%.2f%%** |\n",
			r.repo, r.files, pct(r.withError, r.files), pct(r.typeLoss, r.files))
		tf += r.files
		te += r.withError
		tl += r.typeLoss
	}
	fmt.Fprintf(&b, "\ntotal: %d files, %.2f%% with any error, %.2f%% losing a type\n", tf, pct(te, tf), pct(tl, tf))

	// Where the type losses cluster. A single package accounting for most of a repo's
	// losses is a specific grammar gap worth naming; losses spread evenly across a repo
	// would mean something general and much worse.
	fmt.Fprintf(&b, "\ntype-loss hot spots (dir -> files):\n")
	for _, r := range results {
		if r.typeLoss == 0 {
			continue
		}
		type kv struct {
			d string
			n int
		}
		var kvs []kv
		for d, n := range r.worstDirs {
			kvs = append(kvs, kv{d, n})
		}
		sort.Slice(kvs, func(i, j int) bool {
			if kvs[i].n != kvs[j].n {
				return kvs[i].n > kvs[j].n
			}
			return kvs[i].d < kvs[j].d
		})
		if len(kvs) > 5 {
			kvs = kvs[:5]
		}
		var parts []string
		for _, kv := range kvs {
			parts = append(parts, fmt.Sprintf("%s=%d", kv.d, kv.n))
		}
		fmt.Fprintf(&b, "  %-16s %s\n", r.repo, strings.Join(parts, "  "))
	}
	t.Log(b.String())
}

// losesType reports whether any ERROR/MISSING node in the tree has no enclosing
// class/module/method definition — i.e. the error reached compilation-unit scope, so a
// top-level definition failed to parse rather than a statement inside one.
func losesType(root *sitter.Node) bool {
	var found bool
	var walk func(n *sitter.Node, insideDef bool)
	walk = func(n *sitter.Node, insideDef bool) {
		if found || n == nil {
			return
		}
		if n.IsError() || n.IsMissing() {
			if !insideDef {
				found = true
			}
			return
		}
		switch n.Kind() {
		case "class", "module", "singleton_class", "method", "singleton_method":
			insideDef = true
		}
		for i := uint(0); i < n.ChildCount(); i++ {
			walk(n.Child(i), insideDef)
		}
	}
	walk(root, false)
	return found
}

// topDirs returns the first n path segments of rel, for grouping losses by package.
func topDirs(rel string, n int) string {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) <= n {
		return filepath.ToSlash(filepath.Dir(rel))
	}
	return strings.Join(parts[:n], "/")
}

func pct(n, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(n) * 100 / float64(total)
}
