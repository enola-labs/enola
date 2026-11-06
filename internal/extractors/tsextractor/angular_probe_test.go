package tsextractor

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	sitter "github.com/enola-labs/enola/internal/treesitter"
	typescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
)

// The Angular corpus probes. Both are skipped by default; run them with
//
//	ANGULAR_CORPUS=$HOME/development/angular go test ./internal/extractors/tsextractor/ \
//	    -run 'TestAngularCorpus' -v -timeout 40m
//
// They measure the two things that decide whether an Angular dialect is worth
// writing and how it has to be written, BEFORE any of it exists — the discipline
// the Scala, Dart and Ruby corpora were each given and which cost the ones that
// skipped it.

// TestAngularCorpusParseCoverage measures what the vendored TypeScript grammar
// reads on real Angular repositories, and where its failures land.
//
// Two columns, and only the second matters much: an ERROR inside a method body
// loses that method's call edges, while an error whose nearest ancestor is the
// compilation unit means the class around it never parsed — the type node is gone
// and its members scatter to file scope.
func TestAngularCorpusParseCoverage(t *testing.T) {
	root := os.Getenv("ANGULAR_CORPUS")
	if root == "" {
		t.Skip("corpus probe disabled; set ANGULAR_CORPUS=<dir of sibling Angular repos> to run")
	}

	type result struct {
		repo                       string
		files, withError, typeLoss int
		worstDirs                  map[string]int
	}
	var results []result

	for _, ent := range corpusRepos(t, root) {
		repoPath := filepath.Join(root, ent)
		res := result{repo: ent, worstDirs: map[string]int{}}

		parser := sitter.NewParser()
		if err := parser.SetLanguage(sitter.NewLanguage(typescript.LanguageTypescript())); err != nil {
			parser.Close()
			t.Fatalf("SetLanguage: %v", err)
		}

		walkCorpus(repoPath, func(path, rel string) {
			if strings.ToLower(filepath.Ext(path)) != ".ts" || strings.HasSuffix(path, ".d.ts") {
				return
			}
			src, err := os.ReadFile(path)
			if err != nil || len(src) == 0 || isMinifiedSource(src) {
				return
			}
			res.files++

			tree := parser.Parse(src, nil)
			if tree == nil {
				return
			}
			defer tree.Close()
			rootNode := tree.RootNode()
			if !rootNode.HasError() {
				return
			}
			res.withError++
			if tsLosesType(rootNode) {
				res.typeLoss++
				res.worstDirs[topSegments(rel, 2)]++
			}
		})
		parser.Close()

		if res.files > 0 {
			results = append(results, res)
		}
	}

	sort.Slice(results, func(i, j int) bool {
		return ratio(results[i].typeLoss, results[i].files) < ratio(results[j].typeLoss, results[j].files)
	})

	var b strings.Builder
	fmt.Fprintf(&b, "\n| Repo | Files | Files with any error | **Files losing a type** |\n|---|---:|---:|---:|\n")
	var tf, te, tl int
	for _, r := range results {
		fmt.Fprintf(&b, "| %s | %d | %.2f%% | **%.2f%%** |\n",
			r.repo, r.files, ratio(r.withError, r.files), ratio(r.typeLoss, r.files))
		tf += r.files
		te += r.withError
		tl += r.typeLoss
	}
	fmt.Fprintf(&b, "\ntotal: %d files, %.2f%% with any error, %.2f%% losing a type\n", tf, ratio(te, tf), ratio(tl, tf))

	fmt.Fprintf(&b, "\ntype-loss hot spots (dir -> files):\n")
	for _, r := range results {
		if r.typeLoss == 0 {
			continue
		}
		fmt.Fprintf(&b, "  %-18s %s\n", r.repo, topCounts(r.worstDirs, 5))
	}
	t.Log(b.String())
}

// angularSurface counts one repository's DECLARED Angular surface — the
// denominator every later phase is graded against. "grape has 1,596 verb sites and
// enola extracts 5 routes" is only a readable sentence because someone counted the
// 1,596 first.
type angularSurface struct {
	repo string

	components, directives, pipes, injectables, ngModules int
	standalone                                            int

	templateURL, inlineTemplate int
	htmlFiles                   int
	// Angular 17+ built-in control flow, which any template reader has to handle
	// and which no HTML grammar knows about.
	ctrlFlowFiles int
	deferFiles    int
	structural    int

	forRoot, forChild, provideRouter, loadChildren, loadComponent int
	routerLink, navigateCall                                      int

	ctorInject, inlineInject int
	httpCalls                int
}

var (
	reComponent     = regexp.MustCompile(`@Component\s*\(`)
	reDirective     = regexp.MustCompile(`@Directive\s*\(`)
	rePipe          = regexp.MustCompile(`@Pipe\s*\(`)
	reInjectable    = regexp.MustCompile(`@Injectable\s*\(`)
	reNgModule      = regexp.MustCompile(`@NgModule\s*\(`)
	reStandalone    = regexp.MustCompile(`\bstandalone\s*:\s*true\b`)
	reTemplateURL   = regexp.MustCompile(`\btemplateUrl\s*:`)
	reInlineTmpl    = regexp.MustCompile(`\btemplate\s*:\s*` + "[`'\"]")
	reForRoot       = regexp.MustCompile(`RouterModule\s*\.\s*forRoot\s*\(`)
	reForChild      = regexp.MustCompile(`RouterModule\s*\.\s*forChild\s*\(`)
	reProvideRouter = regexp.MustCompile(`\bprovideRouter\s*\(`)
	reLoadChildren  = regexp.MustCompile(`\bloadChildren\s*:`)
	reLoadComponent = regexp.MustCompile(`\bloadComponent\s*:`)
	reRouterLink    = regexp.MustCompile(`routerLink`)
	reNavigate      = regexp.MustCompile(`\.\s*(?:navigate|navigateByUrl)\s*\(`)
	reCtorInject    = regexp.MustCompile(`(?:private|public|protected|readonly)\s+\w+\s*:\s*[A-Z]\w*`)
	reInlineInj     = regexp.MustCompile(`=\s*inject\s*\(`)
	reHTTPCall      = regexp.MustCompile(`\bhttp\w*\s*\.\s*(?:get|post|put|delete|patch)\s*(?:<[^()]*>)?\s*\(`)
	reCtrlFlow      = regexp.MustCompile(`@(?:if|for|switch|empty|else)\b`)
	reDefer         = regexp.MustCompile(`@defer\b`)
	reStructural    = regexp.MustCompile(`\*ng(?:If|For|SwitchCase|TemplateOutlet)\b`)
)

// TestAngularCorpusSurfaces counts the Angular constructs each repository
// DECLARES, so a later phase's extraction count has something to be a fraction of.
// Lexical on purpose: it is a census, not an extractor, and it must not share code
// with the thing it will grade.
func TestAngularCorpusSurfaces(t *testing.T) {
	root := os.Getenv("ANGULAR_CORPUS")
	if root == "" {
		t.Skip("corpus probe disabled; set ANGULAR_CORPUS=<dir of sibling Angular repos> to run")
	}

	var all []angularSurface
	for _, ent := range corpusRepos(t, root) {
		repoPath := filepath.Join(root, ent)
		s := angularSurface{repo: ent}

		walkCorpus(repoPath, func(path, rel string) {
			ext := strings.ToLower(filepath.Ext(path))
			if ext != ".ts" && ext != ".html" {
				return
			}
			src, err := os.ReadFile(path)
			if err != nil || len(src) == 0 || isMinifiedSource(src) {
				return
			}
			text := string(src)

			if ext == ".html" {
				s.htmlFiles++
				countInto(&s.ctrlFlowFiles, reCtrlFlow, text, true)
				countInto(&s.deferFiles, reDefer, text, true)
				countInto(&s.structural, reStructural, text, false)
				countInto(&s.routerLink, reRouterLink, text, false)
				return
			}
			if strings.HasSuffix(path, ".d.ts") {
				return
			}

			countInto(&s.components, reComponent, text, false)
			countInto(&s.directives, reDirective, text, false)
			countInto(&s.pipes, rePipe, text, false)
			countInto(&s.injectables, reInjectable, text, false)
			countInto(&s.ngModules, reNgModule, text, false)
			countInto(&s.standalone, reStandalone, text, false)
			countInto(&s.templateURL, reTemplateURL, text, false)
			countInto(&s.inlineTemplate, reInlineTmpl, text, false)
			countInto(&s.forRoot, reForRoot, text, false)
			countInto(&s.forChild, reForChild, text, false)
			countInto(&s.provideRouter, reProvideRouter, text, false)
			countInto(&s.loadChildren, reLoadChildren, text, false)
			countInto(&s.loadComponent, reLoadComponent, text, false)
			countInto(&s.routerLink, reRouterLink, text, false)
			countInto(&s.navigateCall, reNavigate, text, false)
			countInto(&s.ctorInject, reCtorInject, text, false)
			countInto(&s.inlineInject, reInlineInj, text, false)
			countInto(&s.httpCalls, reHTTPCall, text, false)
			// An inline template carries the same control flow an external one does.
			countInto(&s.ctrlFlowFiles, reCtrlFlow, text, true)
			countInto(&s.deferFiles, reDefer, text, true)
			countInto(&s.structural, reStructural, text, false)
		})
		all = append(all, s)
	}

	sort.Slice(all, func(i, j int) bool { return all[i].components > all[j].components })

	var b strings.Builder
	fmt.Fprintf(&b, "\nDeclared Angular surface\n\n")
	fmt.Fprintf(&b, "| Repo | @Component | standalone | @Directive | @Pipe | @Injectable | @NgModule |\n|---|---:|---:|---:|---:|---:|---:|\n")
	for _, s := range all {
		fmt.Fprintf(&b, "| %s | %d | %d | %d | %d | %d | %d |\n",
			s.repo, s.components, s.standalone, s.directives, s.pipes, s.injectables, s.ngModules)
	}

	fmt.Fprintf(&b, "\n| Repo | .html | templateUrl | inline template | files w/ @if-@for | files w/ @defer | *ngIf-style |\n|---|---:|---:|---:|---:|---:|---:|\n")
	for _, s := range all {
		fmt.Fprintf(&b, "| %s | %d | %d | %d | %d | %d | %d |\n",
			s.repo, s.htmlFiles, s.templateURL, s.inlineTemplate, s.ctrlFlowFiles, s.deferFiles, s.structural)
	}

	fmt.Fprintf(&b, "\n| Repo | forRoot | forChild | provideRouter | loadChildren | loadComponent | routerLink | navigate() |\n|---|---:|---:|---:|---:|---:|---:|---:|\n")
	for _, s := range all {
		fmt.Fprintf(&b, "| %s | %d | %d | %d | %d | %d | %d | %d |\n",
			s.repo, s.forRoot, s.forChild, s.provideRouter, s.loadChildren, s.loadComponent, s.routerLink, s.navigateCall)
	}

	fmt.Fprintf(&b, "\n| Repo | ctor-injected params | inject() | http verb calls |\n|---|---:|---:|---:|\n")
	for _, s := range all {
		fmt.Fprintf(&b, "| %s | %d | %d | %d |\n", s.repo, s.ctorInject, s.inlineInject, s.httpCalls)
	}
	t.Log(b.String())
}

// countInto adds the number of matches to n, or 1 when perFile is set and the file
// matched at all.
func countInto(n *int, re *regexp.Regexp, text string, perFile bool) {
	if perFile {
		if re.MatchString(text) {
			*n++
		}
		return
	}
	*n += len(re.FindAllStringIndex(text, -1))
}

// corpusRepos lists the repository directories directly under root.
func corpusRepos(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("reading corpus dir %s: %v", root, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// walkCorpus visits every file under repoPath outside build output, vendored code
// and enola's own artifacts, calling fn with the absolute and repo-relative path.
func walkCorpus(repoPath string, fn func(path, rel string)) {
	_ = filepath.WalkDir(repoPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "dist", "build", "out-tsc", "coverage", ".angular", ".nx", ".enola", "bazel-out":
				return filepath.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(repoPath, path)
		if rerr != nil {
			return nil
		}
		fn(path, filepath.ToSlash(rel))
		return nil
	})
}

// tsLosesType reports whether any ERROR/MISSING node has no enclosing type or
// function definition — i.e. the error reached compilation-unit scope.
func tsLosesType(root *sitter.Node) bool {
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
		case "class_declaration", "abstract_class_declaration", "class", "interface_declaration",
			"function_declaration", "function_expression", "method_definition", "enum_declaration":
			insideDef = true
		}
		for i := range n.ChildCount() {
			walk(n.Child(i), insideDef)
		}
	}
	walk(root, false)
	return found
}

// topSegments returns the first n path segments of rel, for grouping by area.
func topSegments(rel string, n int) string {
	parts := strings.Split(rel, "/")
	if len(parts) <= n {
		return filepath.ToSlash(filepath.Dir(rel))
	}
	return strings.Join(parts[:n], "/")
}

// topCounts renders the n largest entries of m as "key=count" pairs.
func topCounts(m map[string]int, n int) string {
	type kv struct {
		k string
		v int
	}
	kvs := make([]kv, 0, len(m))
	for k, v := range m {
		kvs = append(kvs, kv{k, v})
	}
	sort.Slice(kvs, func(i, j int) bool {
		if kvs[i].v != kvs[j].v {
			return kvs[i].v > kvs[j].v
		}
		return kvs[i].k < kvs[j].k
	})
	if len(kvs) > n {
		kvs = kvs[:n]
	}
	parts := make([]string, 0, len(kvs))
	for _, kv := range kvs {
		parts = append(parts, fmt.Sprintf("%s=%d", kv.k, kv.v))
	}
	return strings.Join(parts, "  ")
}

func ratio(n, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(n) * 100 / float64(total)
}
