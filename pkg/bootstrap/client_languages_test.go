package bootstrap_test

import (
	"testing"

	"github.com/enola-labs/enola/internal/clientspec"
)

// A client spec names its language by extractor name. A language that stops being an
// extractor name would turn every spec naming it into a validation error, and a new
// extractor missing from both maps would reject its specs as typos instead of reporting
// that they are not implemented yet.
func TestClientLanguagesAreRegisteredExtractors(t *testing.T) {
	registered := map[string]bool{}
	for _, name := range registeredExtractors(t) {
		registered[name] = true
	}
	for _, languages := range []map[string]bool{clientspec.Languages, clientspec.WithoutReader} {
		for name := range languages {
			if !registered[name] {
				t.Errorf("client language %q is not a registered extractor", name)
			}
			if clientspec.Languages[name] && clientspec.WithoutReader[name] {
				t.Errorf("client language %q is listed both with and without a reader", name)
			}
		}
	}
}
