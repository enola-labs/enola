package bootstrap_test

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/docslint"
	"github.com/enola-labs/enola/pkg/bootstrap"
)

// This is Tier A's second half, and it lives here rather than in internal/docslint for
// one reason: the extractor set is only knowable from a fully wired engine, and wiring
// one pulls in every tree-sitter grammar. docslint's value is that it runs in a git
// hook in milliseconds, so the expensive contract rides the package that already pays
// for the engine.
//
// It is a _test.go file, so it never enters the import graph the layer gate reads —
// Go test files contribute test_ref facts and no dependency edges, which is why
// internal/server/e2e_test.go may import pkg/cli without the declared layer order
// objecting. Nothing here changes what the binary links.

// extractorDoc is how one registered extractor appears in the documentation.
type extractorDoc struct {
	// Labels are the first-column spellings the language tables use. A table
	// satisfies the contract when it carries a row labelled any one of them, which
	// is what lets `typescript` be "TypeScript" in one table and
	// "TypeScript / JavaScript" in another without either being wrong.
	Labels []string
	// Page is the docs/extraction page documenting it. Several extractors may share
	// one. Empty means no page, and NoPageWhy must say why.
	Page string
	// NoPageWhy is required when Page is empty.
	NoPageWhy string
}

// extractorDocs maps every registered extractor to its documented forms.
//
// This map is the alias layer the rest of the doc checks do not need. An explainer is
// called `god-class` in the code and `god-class` in the prose, so a substring match is
// the whole story. An extractor is called `typescript` and written "TypeScript /
// JavaScript"; `cpp` and "C / C++"; `hcl` and "Terraform / HCL". Matching those by
// substring would either miss them or, for `go`, match the word "go" in every
// paragraph on the page.
//
// The map is itself a drift surface, so it is closed at both ends:
// TestExtractorDocsCoversEveryExtractor fails on a registered extractor with no entry,
// and on an entry for an extractor no longer registered.
var extractorDocs = map[string]extractorDoc{
	"go":         {Labels: []string{"Go"}, Page: "go.md"},
	"java":       {Labels: []string{"Java"}, Page: "java.md"},
	"kotlin":     {Labels: []string{"Kotlin"}, Page: "kotlin.md"},
	"python":     {Labels: []string{"Python"}, Page: "python.md"},
	"ruby":       {Labels: []string{"Ruby"}, Page: "ruby.md"},
	"php":        {Labels: []string{"PHP"}, Page: "php.md"},
	"rust":       {Labels: []string{"Rust"}, Page: "rust.md"},
	"scala":      {Labels: []string{"Scala"}, Page: "scala.md"},
	"swift":      {Labels: []string{"Swift"}, Page: "swift.md"},
	"dart":       {Labels: []string{"Dart", "Dart / Flutter"}, Page: "dart.md"},
	"cpp":        {Labels: []string{"C/C++", "C / C++"}, Page: "cpp.md"},
	"dotnet":     {Labels: []string{".NET"}, Page: "dotnet.md"},
	"typescript": {Labels: []string{"TypeScript", "TypeScript / JavaScript"}, Page: "typescript.md"},
	"hcl":        {Labels: []string{"Terraform / HCL"}, Page: "hcl.md"},
	"ansible":    {Labels: []string{"Ansible"}, Page: "ansible.md"},
	"asyncapi":   {Labels: []string{"AsyncAPI"}, Page: "asyncapi.md"},
	"openapi":    {Labels: []string{"OpenAPI", "gRPC and OpenAPI"}, Page: "grpc-openapi.md"},
	"grpc":       {Labels: []string{"gRPC", "gRPC and OpenAPI"}, Page: "grpc-openapi.md"},
	"sql":        {Labels: []string{"SQL"}, Page: "sql.md"},

	"manifests": {
		NoPageWhy: "not a language. It reads the package manifests of seven ecosystems " +
			"for one thing — the DECLARED direct dependencies — and a repository is not " +
			"written in any of them. What it emits and what a declaration may say about " +
			"it is documented in docs/INTENT.md, beside the verdicts that read it.",
	},

	"mdintent": {
		NoPageWhy: "not a language. It compiles enola's own knowledge pages into intent " +
			"facts, so what it reads is documented in docs/INTENT.md — a language table " +
			"row for it would tell a reader their repository is written in mdintent.",
	},
}

// languageTables are the surfaces that promise a complete list of what enola parses.
var languageTables = []struct{ Doc, Section, Why string }{
	{"README.md", "Supported languages",
		"the first place anyone checks whether their stack is covered"},
	{"ARCHITECTURE.md", "Supported languages",
		"the parser used and the exact files that trigger detection"},
	{"docs/extraction/README.md", "",
		"the index into the per-language pages"},
}

// TestEveryExtractorAppearsInEveryLanguageTable is the check that found AsyncAPI
// missing from ARCHITECTURE.md's table — nineteen extractors, eighteen rows, and no
// count anywhere in the prose that would have disagreed with itself.
func TestEveryExtractorAppearsInEveryLanguageTable(t *testing.T) {
	docs := corpusByPath(t)

	for _, name := range registeredExtractors(t) {
		doc, ok := extractorDocs[name]
		if !ok {
			continue // reported by TestExtractorDocsCoversEveryExtractor
		}
		if len(doc.Labels) == 0 {
			continue // deliberately not a language; NoPageWhy covers it
		}
		for _, table := range languageTables {
			d, ok := docs[table.Doc]
			if !ok {
				t.Errorf("languageTables names %s, which is not in the corpus", table.Doc)
				continue
			}
			text := d.Prose
			if table.Section != "" {
				body, found := d.Section(table.Section)
				if !found {
					t.Errorf("%s has no %q section — it was renamed, and the extractor "+
						"contract now checks nothing", table.Doc, table.Section)
					continue
				}
				text = body
			}
			if !hasRow(docslint.LanguageTableRows(text), doc.Labels) {
				t.Errorf("%s's language table has no row for the %q extractor "+
					"(any of: %s).\n    That table exists to give the reader: %s",
					table.Doc, name, strings.Join(doc.Labels, ", "), table.Why)
			}
		}
	}
}

// TestEveryExtractorHasAnExtractionPage checks the fourth surface: the per-language
// page in docs/extraction, which is the one that shows the facts a construct produces.
func TestEveryExtractorHasAnExtractionPage(t *testing.T) {
	docs := corpusByPath(t)
	for _, name := range registeredExtractors(t) {
		doc, ok := extractorDocs[name]
		if !ok {
			continue
		}
		if doc.Page == "" {
			if doc.NoPageWhy == "" {
				t.Errorf("extractorDocs[%q] has no page and no reason — an extractor "+
					"without a page needs one, or an explanation of why it is not a language", name)
			}
			continue
		}
		if _, ok := docs["docs/extraction/"+doc.Page]; !ok {
			t.Errorf("the %q extractor is documented at docs/extraction/%s, which does not exist", name, doc.Page)
		}
	}
}

// TestExtractorDocsCoversEveryExtractor closes the alias map at both ends, so it
// cannot quietly stop covering what it claims to cover.
func TestExtractorDocsCoversEveryExtractor(t *testing.T) {
	registered := map[string]bool{}
	for _, name := range registeredExtractors(t) {
		registered[name] = true
		if _, ok := extractorDocs[name]; !ok {
			t.Errorf("the engine registers the %q extractor, which extractorDocs never mentions — "+
				"add its documented labels and page, or record why it is not a language", name)
		}
	}
	for name := range extractorDocs {
		if !registered[name] {
			t.Errorf("extractorDocs describes %q, which the engine no longer registers", name)
		}
	}
}

// TestExtractionPagesAreAllClaimed is the converse of the page check: a page in
// docs/extraction that no registered extractor points at is documentation for
// something that no longer runs.
func TestExtractionPagesAreAllClaimed(t *testing.T) {
	claimed := map[string]bool{"README.md": true}
	for _, doc := range extractorDocs {
		if doc.Page != "" {
			claimed[doc.Page] = true
		}
	}
	var orphans []string
	for path := range corpusByPath(t) {
		if !strings.HasPrefix(path, "docs/extraction/") {
			continue
		}
		if base := filepath.Base(path); !claimed[base] {
			orphans = append(orphans, base)
		}
	}
	sort.Strings(orphans)
	for _, o := range orphans {
		t.Errorf("docs/extraction/%s describes an extractor nothing registers", o)
	}
}

func hasRow(rows []string, labels []string) bool {
	for _, row := range rows {
		for _, label := range labels {
			if strings.EqualFold(row, label) {
				return true
			}
		}
	}
	return false
}

func registeredExtractors(t *testing.T) []string {
	t.Helper()
	eng, _, err := bootstrap.NewEngine(bootstrap.Options{
		ConfigPath: filepath.Join(t.TempDir(), "no-such-config.yaml"),
	})
	if err != nil {
		t.Fatalf("building an engine: %v", err)
	}
	var names []string
	for _, e := range eng.Extractors() {
		names = append(names, e.Name())
	}
	if len(names) < 15 {
		t.Fatalf("engine registered %d extractors (%v); expected the full OSS set", len(names), names)
	}
	sort.Strings(names)
	return names
}

func corpusByPath(t *testing.T) map[string]docslint.Doc {
	t.Helper()
	docs, err := docslint.Corpus()
	if err != nil {
		t.Fatalf("reading the documentation corpus: %v", err)
	}
	out := make(map[string]docslint.Doc, len(docs))
	for _, d := range docs {
		out[d.Path] = d
	}
	return out
}
