package constraints

import (
	"context"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// The move advice names the part a breaching fact's other edges reach most. When
// that is the part the fact already sits in, there is nowhere to move it, and the
// advice must not tell the reader to move it into itself.
func TestCutForEdge_NoMoveIntoTheFactsOwnPart(t *testing.T) {
	sym := func(name, file string, calls ...string) facts.Fact {
		f := facts.Fact{Kind: facts.KindSymbol, Name: name, File: file}
		for _, c := range calls {
			f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelCalls, Target: c})
		}
		return f
	}
	store := facts.NewStore()
	store.Add(
		componentIntent("ext", "ext/**"),
		componentIntent("link", "link/**"),
		componentIntent("other", "other/**"),
		ruleIntentProps("ext-no-link", map[string]any{"forbid": "ext", "to": "link", "via": "calls", "because": "test"}),
		sym("ext.Home", "ext/home.go", "ext.B", "ext.C", "link.L"),
		sym("ext.Stray", "ext/stray.go", "other.X", "other.Y", "link.L"),
		sym("ext.B", "ext/b.go"),
		sym("ext.C", "ext/c.go"),
		sym("other.X", "other/x.go"),
		sym("other.Y", "other/y.go"),
		sym("link.L", "link/l.go"),
	)
	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]string{}
	for _, in := range insights {
		if RuleIDFromTitle(in.Title) == "ext-no-link" && len(in.Evidence) > 0 {
			actions[in.Evidence[0].Symbol] = strings.Join(in.Actions, " | ")
		}
	}
	if got := actions["ext.Home"]; got == "" || strings.Contains(got, "Move ext.Home into") {
		t.Errorf("ext.Home already sits in ext: actions %q", got)
	}
	if got := actions["ext.Stray"]; !strings.Contains(got, "Move ext.Stray into other") {
		t.Errorf("ext.Stray's other edges go to other: actions %q", got)
	}
}
