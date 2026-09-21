// Package phpextractor extracts architectural facts from PHP source code using
// the tree-sitter PHP grammar, in line with the other language extractors. It
// emits symbols (classes, interfaces, traits, enums, functions, methods,
// constants), `use` import dependencies, a call / instantiation graph, inheritance
// edges, and per-function cyclomatic complexity. Outbound HTTP-client calls (Guzzle,
// the Laravel Http facade, Symfony HttpClient, cURL, file_get_contents) become
// client-role route facts. Framework awareness adds server-route facts: WordPress
// hooks (add_action / add_filter / do_action / apply_filters / register_rest_route),
// Laravel's Route:: DSL (verbs, match/any, resource expansion, group prefixes), and
// Symfony routes from #[Route] attributes / @Route annotations and YAML/XML config.
package phpextractor

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/enola-labs/enola/internal/extractors/detectnames"
	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/parallel"
)

// PHPExtractor extracts architectural facts from PHP source code.
type PHPExtractor struct{}

// New creates a new PHPExtractor.
func New() *PHPExtractor {
	return &PHPExtractor{}
}

func (e *PHPExtractor) Name() string {
	return "php"
}

// wordpressMarkers are bootstrap files unique to a WordPress install/core. Their
// presence both confirms a PHP project and enables WordPress hook awareness.
var wordpressMarkers = []string{"wp-load.php", "wp-settings.php", "wp-config.php", "wp-blog-header.php"}

// Detect returns true if the repository looks like a PHP project: it has a
// composer.json, a WordPress bootstrap file, or any .php file within a few
// directory levels (generic PHP projects often ship neither manifest).
func (e *PHPExtractor) Detect(repoPath string) (bool, error) {
	if _, err := os.Stat(filepath.Join(repoPath, "composer.json")); err == nil {
		return true, nil
	}
	for _, m := range wordpressMarkers {
		if _, err := os.Stat(filepath.Join(repoPath, m)); err == nil {
			return true, nil
		}
	}
	return e.DetectFiles(repoPath, detectnames.Walk(repoPath))
}

// DetectFiles implements plugin.FileListDetector. The root-marker fast paths above
// stay stat-based (a root file is one syscall either way); what changes is the
// fallback, which was a three-level scan and is now membership over every walked
// name — so a Gemfile-less, composer-less PHP tree is found wherever it lives.
func (e *PHPExtractor) DetectFiles(repoPath string, files []string) (bool, error) {
	if _, err := os.Stat(filepath.Join(repoPath, "composer.json")); err == nil {
		return true, nil
	}
	for _, m := range wordpressMarkers {
		if _, err := os.Stat(filepath.Join(repoPath, m)); err == nil {
			return true, nil
		}
	}
	for _, rel := range files {
		if isPHPFile(detectnames.Base(rel)) {
			return true, nil
		}
	}
	return false, nil
}

// detectWordPress reports whether the repository is a WordPress codebase, which
// turns on hook (add_action / add_filter / …) route extraction.
func detectWordPress(repoPath string) bool {
	for _, m := range wordpressMarkers {
		if _, err := os.Stat(filepath.Join(repoPath, m)); err == nil {
			return true
		}
	}
	// WordPress core keeps its bootstrap files under src/ in the develop checkout.
	for _, m := range wordpressMarkers {
		if _, err := os.Stat(filepath.Join(repoPath, "src", m)); err == nil {
			return true
		}
	}
	return false
}

// Extract parses PHP files and emits architectural facts.
func (e *PHPExtractor) Extract(ctx context.Context, repoPath string, files []string) ([]facts.Fact, error) {
	fw := detectPHPFramework(repoPath)
	isWordPress := fw == frameworkWordPress

	var phpFiles []string
	for _, relFile := range files {
		if isPHPFile(relFile) {
			phpFiles = append(phpFiles, relFile)
		}
	}

	// Per-file parsing is independent. Parse in parallel and merge in file order
	// for deterministic output.
	perFileFacts := parallel.MapFiles(ctx, phpFiles, func(relFile string) []facts.Fact {
		src, err := os.ReadFile(filepath.Join(repoPath, relFile))
		if err != nil {
			log.Printf("[php-extractor] error reading %s: %v", relFile, err)
			return nil
		}
		// extractFileAST emits symbols, use-imports, inheritance, calls, and
		// complexity. extractPHPHTTPClientFacts adds outbound HTTP-client routes
		// (framework-independent). The framework switch adds server routes.
		ff := extractFileAST(src, relFile)
		ff = append(ff, extractPHPHTTPClientFacts(src, relFile)...)
		switch fw {
		case frameworkWordPress:
			ff = append(ff, extractHooks(src, relFile)...)
		case frameworkLaravel:
			if isLaravelRouteFile(relFile) {
				ff = append(ff, extractLaravelRoutes(src, relFile)...)
			}
		case frameworkSymfony:
			ff = append(ff, extractSymfonyRoutes(src, relFile)...)
		}
		return ff
	})

	var allFacts []facts.Fact
	modules := make(map[string]bool)
	for i, ff := range perFileFacts {
		allFacts = append(allFacts, ff...)
		modules[factpath.Dir(phpFiles[i])] = true
	}

	// Symfony YAML/XML route config lives outside the PHP source tree and is
	// discovered directly on disk (it is commonly hidden by a **/*.yaml ignore),
	// so it is parsed once per repo rather than per PHP file.
	if fw == frameworkSymfony {
		allFacts = append(allFacts, extractSymfonyConfigRoutes(repoPath)...)
	}

	// Emit one module fact per directory containing PHP files.
	for dir := range modules {
		props := map[string]any{"language": "php"}
		if fw != frameworkPlain {
			props["framework"] = string(fw)
		}
		allFacts = append(allFacts, facts.Fact{
			Kind:  facts.KindModule,
			Name:  dir,
			File:  dir,
			Props: props,
		})
	}

	// Resolve `use` imports and namespaced references (inheritance, calls,
	// instantiations) into internal module-coupling edges. Without this, PHP
	// imports never match module Names downstream and coupling collapses to zero.
	allFacts = append(allFacts, resolveImports(allFacts, isWordPress)...)

	// Propagate the direct-I/O signal (io_direct) transitively over the call graph
	// so a function that reaches a DB/HTTP call through a wrapper is also performs_io
	// — lets the performance analyzer recognize an in-loop wrapper call as an N+1.
	computePhpPerformsIO(allFacts)

	return allFacts, nil
}

// computePhpPerformsIO seeds performs_io from io_direct and propagates it over
// RelCalls edges whose target is a known symbol Name, via a monotone false->true
// fixpoint (a verbatim port of the Python/Rust extractors' pass). WordPress core
// global functions carry bare Names and bare call targets, so the exists-gated
// match propagates cleanly; namespaced/class code matches where the names align.
func computePhpPerformsIO(allFacts []facts.Fact) {
	exists := make(map[string]bool)
	for i := range allFacts {
		if allFacts[i].Kind == facts.KindSymbol {
			exists[allFacts[i].Name] = true
		}
	}

	io := make(map[string]bool)      // name -> performs I/O (directly or transitively)
	adj := make(map[string][]string) // name -> called names that are known symbols
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind != facts.KindSymbol {
			continue
		}
		if b, _ := f.PropAny("io_direct").(bool); b {
			io[f.Name] = true
		}
		seen := make(map[string]bool)
		for _, r := range f.Relations {
			if r.Kind != facts.RelCalls || r.Target == f.Name || seen[r.Target] || !exists[r.Target] {
				continue
			}
			seen[r.Target] = true
			adj[f.Name] = append(adj[f.Name], r.Target)
		}
	}

	for changed := true; changed; {
		changed = false
		for name, callees := range adj {
			if io[name] {
				continue
			}
			for _, c := range callees {
				if io[c] {
					io[name] = true
					changed = true
					break
				}
			}
		}
	}

	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind == facts.KindSymbol && io[f.Name] {
			if f.Props == nil {
				f.Props = map[string]any{}
			}
			f.SetProp("performs_io", true)
		}
	}
}

// OwnsFile implements plugin.FileOwner for incremental caching.
func (e *PHPExtractor) OwnsFile(relFile string) bool { return isPHPFile(relFile) }

// isPHPFile returns true if the path has a PHP extension.
func isPHPFile(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".php", ".phtml", ".php4", ".php5", ".php7", ".phps":
		return true
	}
	return false
}
