package facts

import "testing"

func TestClientReach_ThreeStates(t *testing.T) {
	route := func(props map[string]any) Fact {
		return Fact{Kind: KindRoute, Name: "/api/x", Props: props}
	}
	cases := []struct {
		name string
		f    Fact
		want Reach
	}{
		{"reached from an entry point", route(map[string]any{PropClientReachable: true}), ReachRuns},
		{"nothing that runs reaches it", route(map[string]any{PropClientReachable: false}), ReachNeverRuns},
		{"the extractor could not tell", route(map[string]any{PropRole: RoleClient}), ReachUnjudged},
		{"no props at all", route(nil), ReachUnjudged},
		{"a value that is no verdict", route(map[string]any{PropClientReachable: "yes"}), ReachUnjudged},
	}
	for _, c := range cases {
		if got := ClientReach(c.f); got != c.want {
			t.Errorf("%s: ClientReach = %v, want %v", c.name, got, c.want)
		}
	}
}
