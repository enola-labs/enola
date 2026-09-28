package callsite

import (
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// The narrowest span containing a hand-written call names it; a call outside every
// span, and a route that is not a hand-written client call, get nothing.
func TestAttributeSpans_NarrowestSpanWins(t *testing.T) {
	route := func(line int, source string) facts.Fact {
		return facts.Fact{Kind: facts.KindRoute, Name: "/x", Line: line,
			Props: map[string]any{facts.PropRole: facts.RoleClient, facts.PropSource: source}}
	}
	result := []facts.Fact{
		route(5, facts.RouteSourceTSHTTPClient),
		route(20, facts.RouteSourceTSHTTPClient),
		route(5, facts.RouteSourceOpenAPI),
	}
	AttributeSpans([]Span{
		{Start: 0, End: 10, Symbol: "outer"},
		{Start: 3, End: 6, Symbol: "inner"},
	}, result)
	if got := result[0].PropString(facts.PropCaller); got != "inner" {
		t.Errorf("caller = %q, want the narrowest span", got)
	}
	if got := result[1].PropString(facts.PropCaller); got != "" {
		t.Errorf("a call outside every span got caller %q", got)
	}
	if got := result[2].PropString(facts.PropCaller); got != "" {
		t.Errorf("a generated-spec route got caller %q", got)
	}
}
