package resolve

import (
	"slices"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A name written as code writes it resolves to the fact enola stores it under: a Go
// package by its package clause, including a module's root package ("."), and a
// dotted module path by its slashed one.
func TestSpellings_NameAsCodeWritesIt(t *testing.T) {
	store := storeWith(
		facts.Fact{Kind: facts.KindModule, Name: ".", Props: map[string]any{"package": "gin"}},
		facts.Fact{Kind: facts.KindModule, Name: "internal/server", Props: map[string]any{"package": "server"}},
		facts.Fact{Kind: facts.KindModule, Name: "render", Props: map[string]any{"package": "render"}},
		facts.Fact{Kind: facts.KindSymbol, Name: "..Engine.ServeHTTP", File: "gin.go"},
		facts.Fact{Kind: facts.KindSymbol, Name: "internal/server.Server", File: "internal/server/server.go"},
		facts.Fact{Kind: facts.KindSymbol, Name: "render.JSON", File: "render/json.go"},
		facts.Fact{Kind: facts.KindSymbol, Name: "django/db/models/query.QuerySet.__iter__", File: "django/db/models/query.py"},
	)
	r := Resolver{Store: store}
	cases := map[string][]string{
		"gin.Engine.ServeHTTP":                     {"..Engine.ServeHTTP"},
		"server.Server":                            {"internal/server.Server"},
		"django.db.models.query.QuerySet.__iter__": {"django/db/models/query.QuerySet.__iter__"},
		// Stored as written: nothing to respell.
		"render.JSON": nil,
		// No fact carries the respelled name: no guess.
		"gin.Engine.Missing": nil,
		"other.Engine":       nil,
		"repo:x gin.Engine":  nil,
	}
	for in, want := range cases {
		if got := r.Spellings(store, in); !slices.Equal(got, want) {
			t.Errorf("Spellings(%q) = %v, want %v", in, got, want)
		}
	}

	for in, want := range map[string]string{
		"gin.Engine.ServeHTTP":                     "..Engine.ServeHTTP",
		"django.db.models.query.QuerySet.__iter__": "django/db/models/query.QuerySet.__iter__",
	} {
		got, res, err := r.NodeName(in)
		if err != nil || got != want || res != nil {
			t.Errorf("NodeName(%q) = %q, %v, %v; want %q", in, got, res, err, want)
		}
	}
}

// Two packages with one name, neither found by a substring search, both respell and
// are ranked like any two candidates: one picked, flagged ambiguous, the other offered.
func TestSpellings_SamePackageNameTwiceIsAmbiguous(t *testing.T) {
	store := storeWith(
		facts.Fact{Kind: facts.KindModule, Name: ".", Props: map[string]any{"package": "tool"}},
		facts.Fact{Kind: facts.KindModule, Name: "cmd/main", Props: map[string]any{"package": "tool"}},
		facts.Fact{Kind: facts.KindSymbol, Name: "..Handler", File: "h.go"},
		facts.Fact{Kind: facts.KindSymbol, Name: "cmd/main.Handler", File: "cmd/main/h.go"},
	)
	got, res, err := Resolver{Store: store}.NodeName("tool.Handler")
	if err != nil || res == nil || !res.Ambiguous {
		t.Fatalf("NodeName(tool.Handler) = %q, %+v, %v; want an ambiguous resolution", got, res, err)
	}
	alts := got + "," + strings.Join(res.Alternatives, ",")
	if !strings.Contains(alts, "..Handler") || !strings.Contains(alts, "cmd/main.Handler") {
		t.Errorf("alternatives %v, want both handlers", res.Alternatives)
	}
}
