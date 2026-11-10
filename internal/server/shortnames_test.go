package server

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func goModule(name, file, repo string) facts.Fact {
	return facts.Fact{Kind: facts.KindModule, Name: name, File: file, Repo: repo, Props: map[string]any{"language": "go"}}
}

func shortStore(ff ...facts.Fact) *facts.Store {
	s := facts.NewStore()
	s.Add(ff...)
	return s
}

func TestShortener_OneRepository(t *testing.T) {
	sh := newShortener(shortStore(
		goModule("github.com/acme/app", ".", "app"),
		goModule("github.com/acme/app/internal/server", "internal/server", "app"),
		goModule("github.com/acme/app/internal/log", "internal/log", "app"),
	))
	in := "- github.com/acme/app/internal/server.Server.Run (symbol), see github.com/acme/app/internal/server.\n" +
		"root: github.com/acme/app.main; other module: github.com/acme/app-ext/x.Y; stdlib: net/http.Client"
	got, mods := sh.shorten(in)
	want := "- internal/server.Server.Run (symbol), see internal/server.\n" +
		"root: github.com/acme/app.main; other module: github.com/acme/app-ext/x.Y; stdlib: net/http.Client"
	if got != want {
		t.Errorf("shorten:\n got %q\nwant %q", got, want)
	}
	if len(mods) != 1 || mods[0] != "github.com/acme/app" {
		t.Errorf("modules = %v, want [github.com/acme/app]", mods)
	}
	if got, mods := sh.shorten("no Go names here"); got != "no Go names here" || mods != nil {
		t.Errorf("a text with no Go name changed: %q %v", got, mods)
	}
}

// Shortening must never make a name read as something else: a directory two loaded
// repositories share, a directory named like a standard library package the code
// imports, and a short form some other fact carries all stay in full.
func TestShortener_KeepsWhatWouldBeAmbiguous(t *testing.T) {
	dep := facts.Fact{Kind: facts.KindDependency, Name: "github.com/acme/a/svc -> log", File: "a/svc/s.go", Repo: "a",
		Props: map[string]any{"source": "stdlib"}, Relations: []facts.Relation{{Kind: facts.RelImports, Target: "log"}}}
	sh := newShortener(shortStore(
		goModule("github.com/acme/a/internal/config", "a/internal/config", "a"),
		goModule("github.com/acme/b/internal/config", "b/internal/config", "b"),
		goModule("github.com/acme/a/log", "a/log", "a"),
		goModule("github.com/acme/a/web", "a/web", "a"),
		facts.Fact{Kind: facts.KindSymbol, Name: "web.Render", File: "web/render.ts", Repo: "a"},
		goModule("github.com/acme/a/svc", "a/svc", "a"),
		goModule("github.com/acme/a/cgo", "a/cgo", "a"),
		facts.Fact{Kind: facts.KindModule, Name: "cgo", File: "cgo", Repo: "a", Props: map[string]any{"language": "c"}},
		dep,
	))
	for in, want := range map[string]string{
		"github.com/acme/a/internal/config.Load": "github.com/acme/a/internal/config.Load", // two repositories
		"github.com/acme/a/log.Printf":           "github.com/acme/a/log.Printf",           // the standard library's log
		"github.com/acme/a/web.Render":           "github.com/acme/a/web.Render",           // another fact's name
		"github.com/acme/a/web.Serve":            "web.Serve",
		"github.com/acme/a/cgo":                  "github.com/acme/a/cgo", // the C module beside it is named cgo
		"github.com/acme/a/cgo.Node.Child":       "cgo.Node.Child",        // no other fact is
		"github.com/acme/a/svc.Run":              "svc.Run",
	} {
		if got, _ := sh.shorten(in); got != want {
			t.Errorf("shorten(%q) = %q, want %q", in, got, want)
		}
	}
}
