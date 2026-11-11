// Package vespaextractor reads Vespa schema definitions (.sd) as storage.
//
// A Vespa application declares each document type it stores in a schema file:
//
//	schema items_uk inherits items {
//	    document items_uk inherits items { }
//	}
//
// Each document type is a storage fact named by the type, storage_kind
// "document_type", engine "vespa". That is the name a YQL query gives it
// (`select * from items_uk where ...`), which the extractors already read as a
// table reference, so a query and the schema it reads meet on one node. A type it
// inherits from is recorded in the inherits prop.
package vespaextractor

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/enola-labs/enola/internal/extractors/detectnames"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/linescan"
)

// Extractor reads Vespa schema files.
type Extractor struct{}

// New creates the extractor.
func New() *Extractor { return &Extractor{} }

// Name returns the extractor name.
func (e *Extractor) Name() string { return "vespa" }

// OwnsFile scopes caching to schema files.
func (e *Extractor) OwnsFile(relFile string) bool { return isSchemaFile(relFile) }

func isSchemaFile(path string) bool { return strings.EqualFold(filepath.Ext(path), ".sd") }

// Detect reports a schema file anywhere in the repository.
func (e *Extractor) Detect(repoPath string) (bool, error) {
	return e.DetectFiles(repoPath, detectnames.Walk(repoPath))
}

// DetectFiles implements plugin.FileListDetector.
func (e *Extractor) DetectFiles(_ string, files []string) (bool, error) {
	for _, rel := range files {
		if isSchemaFile(rel) {
			return true, nil
		}
	}
	return false, nil
}

// documentDecl is `document <name>` with an optional `inherits a, b`, at the start of
// a line once comments are stripped.
var documentDecl = regexp.MustCompile(`^\s*document\s+([A-Za-z_][A-Za-z0-9_]*)(?:\s+inherits\s+([A-Za-z0-9_,\s]+?))?\s*\{`)

// Extract emits a storage fact per document type the schema files declare.
func (e *Extractor) Extract(ctx context.Context, repoPath string, files []string) ([]facts.Fact, error) {
	var out []facts.Fact
	for _, rel := range files {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if !isSchemaFile(rel) {
			continue
		}
		f, err := os.Open(filepath.Join(repoPath, rel))
		if err != nil {
			continue
		}
		sc := linescan.New(f)
		for line := 1; sc.Scan(); line++ {
			text := sc.Text()
			if i := strings.Index(text, "#"); i >= 0 {
				text = text[:i] // a Vespa comment runs to the end of the line
			}
			m := documentDecl.FindStringSubmatch(text)
			if m == nil {
				continue
			}
			props := map[string]any{"storage_kind": "document_type", "engine": "vespa", "language": "vespa"}
			var bases []string
			for _, b := range strings.Split(m[2], ",") {
				if b = strings.TrimSpace(b); b != "" {
					bases = append(bases, b)
				}
			}
			if len(bases) > 0 {
				props["inherits"] = bases
			}
			out = append(out, facts.Fact{Kind: facts.KindStorage, Name: m[1], File: rel, Line: line, Props: props})
		}
		_ = f.Close()
	}
	return out, nil
}
