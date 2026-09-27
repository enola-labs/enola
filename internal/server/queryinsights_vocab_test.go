package server

import (
	"reflect"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/config"
)

// TestQueryInsightsNamesEveryExplainer holds the explainer= vocabulary on the
// parameter's schema, its one full listing, in step with the vocabulary itself.
//
// The jsonschema struct tag can drift: a Go tag is a string literal, there is nowhere to
// interpolate, and it had gone stale exactly that way — naming eleven explainers
// while sixteen shipped. An agent reads the schema to decide what it may ask for, so
// a name missing from the tag is a filter nobody knows exists.
func TestQueryInsightsNamesEveryExplainer(t *testing.T) {
	field, ok := reflect.TypeOf(queryInsightsArgs{}).FieldByName("Explainer")
	if !ok {
		t.Fatal("queryInsightsArgs has no Explainer field")
	}
	tag := field.Tag.Get("jsonschema")

	for _, name := range config.KnownExplainers {
		if !strings.Contains(tag, name) {
			t.Errorf("the explainer= jsonschema tag never names %q, which config.KnownExplainers ships — "+
				"an agent reading the schema cannot know the filter accepts it", name)
		}
	}

	// The converse: a name in the tag that no build runs is a filter that silently
	// matches nothing.
	known := map[string]bool{}
	for _, name := range config.KnownExplainers {
		known[name] = true
	}
	for _, word := range strings.FieldsFunc(tag, func(r rune) bool {
		return r == ',' || r == ' ' || r == '.' || r == ':'
	}) {
		if strings.Contains(word, "-") && !known[word] {
			t.Errorf("the explainer= jsonschema tag names %q, which config.KnownExplainers does not ship", word)
		}
	}
}

// TestExplainerHintsAreLive refuses an annotation for an explainer that no longer
// exists — a hint nothing renders is a stale note pretending to be documentation.
func TestExplainerHintsAreLive(t *testing.T) {
	known := map[string]bool{}
	for _, name := range config.KnownExplainers {
		known[name] = true
	}
	for name := range explainerHints {
		if !known[name] {
			t.Errorf("explainerHints annotates %q, which config.KnownExplainers no longer ships", name)
		}
	}
}
