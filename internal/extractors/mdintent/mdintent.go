// Package mdintent extracts declared intent from markdown pages — the wiki as
// a source tree, compiled by enola like any other source. A page opts in with
// an `enola_intent:` frontmatter key (namespaced so the wiki's own toolchain
// ignores it); the page body is prose and is never parsed. Every fact this
// extractor emits cites the PAGE as its file, so a verdict's evidence points
// at the decision that declared the intent, not at a config artifact.
package mdintent

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/enola-labs/enola/internal/extractors/extcoverage"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/intent"
	"github.com/enola-labs/enola/internal/workspace"
	"github.com/enola-labs/enola/pkg/plugin"
)

// Extractor extracts enola_intent frontmatter from markdown pages.
type Extractor struct{}

// New creates the extractor.
func New() *Extractor { return &Extractor{} }

// Name returns the extractor name.
func (e *Extractor) Name() string { return "mdintent" }

func (e *Extractor) OwnsFactFile(relFile string) bool {
	return strings.HasSuffix(relFile, ".md")
}

// Detect probes for any markdown file up to five directory levels (wiki trees
// nest: wiki/<scope>/permanent/<area>/page.md). A page carrying the
// enola_intent key compiles as intent; every other markdown file is a
// document, so a repository with a README is in scope.
func (e *Extractor) Detect(repoPath string) (bool, error) {
	found := false
	root := filepath.Clean(repoPath) //factpath:host
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || found {
			return filepath.SkipAll
		}
		if d.IsDir() {
			name := d.Name()
			if path != root && (strings.HasPrefix(name, ".") || mdSkipDirs[name] || workspace.IsLinkedWorktree(path)) {
				return filepath.SkipDir
			}
			rel, _ := filepath.Rel(root, path)
			if rel != "." && strings.Count(filepath.ToSlash(rel), "/") >= 5 {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".md") {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found, nil
}

var mdSkipDirs = map[string]bool{
	"node_modules": true, "vendor": true, "testdata": true, "dist": true,
	"build": true, "tmp": true, "_archive": true, "_views": true,
}

// Extract runs in two modes per file. A page carrying enola_intent compiles
// its block into intent facts; an invalid block fails the snapshot, since a
// declaration that cannot be trusted is worse than none. Every other markdown
// file yields a document symbol, a section symbol per heading and a names
// relation per link that resolves against the walked file set, with the links
// that do not counted on the extraction fact.
func (e *Extractor) Extract(ctx context.Context, repoPath string, files []string) ([]facts.Fact, error) {
	var out []facts.Fact
	var links linkCount
	documents, sections := 0, 0
	// What a link may name: the files the walker kept, and their directories. See
	// inScope — resolving against the filesystem instead made the snapshot depend on
	// whether a previous snapshot existed.
	scope := newInScope(files)
	for _, relFile := range files {
		slashed := filepath.ToSlash(relFile)
		if !strings.HasSuffix(slashed, ".md") ||
			strings.Contains(slashed, "_archive/") || strings.Contains(slashed, "_views/") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(repoPath, relFile))
		if err != nil {
			continue
		}
		page, err := intent.ParsePage(src)
		if err != nil {
			// Fatal, not recorded: an ordinary extractor error loses one file's
			// facts, but losing a declaration loses every verdict computed from it
			// while the snapshot still reports success. The repo-file carrier fails
			// the run for the same reason; both carriers must behave alike, or which
			// file you put a declaration in decides whether a typo is silent.
			return nil, plugin.Fatalf("%s: %w", relFile, err)
		}
		if page == nil {
			docs := documentFacts(scope, relFile, src, &links)
			out = append(out, docs...)
			documents++
			sections += len(docs) - 1
			continue
		}
		out = append(out, intent.CompilePageFacts(page, slashed)...)
		rels := 0
		if page.Page != nil {
			rels = len(page.Page.Relations)
		}
		log.Printf("[mdintent] %s: page=%t %d relation(s), %d seam(s), %d layer set(s), %d claim(s)",
			relFile, page.Page != nil, rels, len(page.Consumes), len(page.Layers), len(page.Claims))
	}
	if documents > 0 {
		unresolved := 0
		for _, n := range links.unresolved {
			unresolved += n
		}
		log.Printf("[mdintent] %d document(s), %d section(s), %d link(s) resolved on disk, %d not", documents, sections, links.resolved, unresolved)
	}
	if coverage, ok := extcoverage.Fact(repoPath, linksFactName, linksEdgeType, links.resolved, links.unresolved); ok {
		out = append(out, coverage)
	}
	return out, nil
}
