package vespaextractor

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// Each document type a schema declares is a storage fact named by the type, with the
// types it inherits; a commented-out declaration and a field are not.
func TestExtract_DocumentTypes(t *testing.T) {
	repo := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("app/schemas/books.sd", `schema books {
    # document drafts {
    document books {
        field title type string {
            indexing: summary | index
        }
        field document type string {
            indexing: attribute
        }
    }
}
`)
	write("app/schemas/books_uk.sd", `schema books_uk inherits books {
    document books_uk inherits books { }
}
`)
	write("app/services.xml", `<document type="books"/>`)

	ex := New()
	ok, err := ex.DetectFiles(repo, []string{"app/schemas/books.sd", "app/services.xml"})
	if err != nil || !ok {
		t.Fatalf("DetectFiles = %v, %v; want a repository with a schema detected", ok, err)
	}
	got, err := ex.Extract(context.Background(), repo, []string{"app/schemas/books.sd", "app/schemas/books_uk.sd", "app/services.xml"})
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]facts.Fact{}
	for _, f := range got {
		byName[f.Name] = f
	}
	if len(byName) != 2 {
		t.Fatalf("got %d document types %v, want books and books_uk", len(byName), byName)
	}
	books := byName["books"]
	if books.Kind != facts.KindStorage || books.PropString("engine") != "vespa" || books.PropString("storage_kind") != "document_type" ||
		books.File != "app/schemas/books.sd" || books.Line != 3 {
		t.Errorf("books = %+v", books)
	}
	if inh, _ := byName["books_uk"].Props["inherits"].([]string); !slices.Equal(inh, []string{"books"}) {
		t.Errorf("books_uk inherits %v, want [books]", byName["books_uk"].Props["inherits"])
	}
}
