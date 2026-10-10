package goextractor

import (
	"slices"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func goScalingCalls(f facts.Fact) []string {
	s, _ := f.PropAny("calls_in_scaling_loop").([]string)
	return s
}

// A call on the path that leaves the loop runs at most once. The call on the path
// that stays in it runs per element.
func TestGoCallOnThePathThatLeavesTheLoopIsNotPerElement(t *testing.T) {
	syms := extractGoModule(t, map[string]string{
		"api/api.go": `package api

import "net/http"

func Check(w http.ResponseWriter, ids []string) {
	for _, id := range ids {
		if id == "" {
			http.Error(w, "empty id", 400)
			return
		}
		if len(id) > 64 {
			report(id)
			break
		}
		store(id)
	}
}

func Must(ids []string) {
	for _, id := range ids {
		if id == "" {
			cleanup(id)
			panic("empty")
		}
	}
}

func report(string)  {}
func store(string)   {}
func cleanup(string) {}
`,
	})
	check := goScalingCalls(syms["api.Check"])
	for _, once := range []string{"net/http.Error", "api.report"} {
		if slices.Contains(check, once) {
			t.Errorf("%s runs at most once and is listed as per-element: %v", once, check)
		}
	}
	if !slices.Contains(check, "api.store") {
		t.Errorf("store runs per element and is not listed: %v", check)
	}
	if must := goScalingCalls(syms["api.Must"]); slices.Contains(must, "api.cleanup") {
		t.Errorf("cleanup precedes a panic and is listed as per-element: %v", must)
	}
}

// A break inside a switch leaves the switch. The loop goes on, and so does the call.
func TestGoBreakInsideSwitchDoesNotLeaveTheLoop(t *testing.T) {
	syms := extractGoModule(t, map[string]string{
		"api/api.go": `package api

func Route(kinds []int) {
	for _, k := range kinds {
		switch k {
		case 1:
			send(k)
			break
		case 2:
			drop(k)
			return
		}
		if k > 9 {
			continue
		}
	}
}

func send(int) {}
func drop(int) {}
`,
	})
	calls := goScalingCalls(syms["api.Route"])
	if !slices.Contains(calls, "api.send") {
		t.Errorf("send is followed by a break out of the switch only, and was dropped: %v", calls)
	}
	if slices.Contains(calls, "api.drop") {
		t.Errorf("drop is followed by a return and is listed as per-element: %v", calls)
	}
}

// The call in a nested loop is not covered by the outer loop's exit.
func TestGoNestedLoopCallBeforeAnOuterExitStillRepeats(t *testing.T) {
	syms := extractGoModule(t, map[string]string{
		"api/api.go": `package api

func Flush(groups [][]int) {
	for _, g := range groups {
		if len(g) > 100 {
			for _, x := range g {
				write(x)
			}
			return
		}
	}
}

func write(int) {}
`,
	})
	if calls := goScalingCalls(syms["api.Flush"]); !slices.Contains(calls, "api.write") {
		t.Errorf("write runs once per element of g and was dropped: %v", calls)
	}
}

// "First match": the body ends in a return, and the search at its top still runs
// for every element that does not match.
func TestGoFirstMatchLoopStillCallsPerElement(t *testing.T) {
	syms := extractGoModule(t, map[string]string{
		"api/api.go": `package api

func Find(principals []string) string {
	for _, p := range principals {
		key, err := search(p)
		if err != nil {
			continue
		}
		return key
	}
	return ""
}

func search(string) (string, error) { return "", nil }
`,
	})
	if calls := goScalingCalls(syms["api.Find"]); !slices.Contains(calls, "api.search") {
		t.Errorf("search runs once per principal until one matches, and was dropped: %v", calls)
	}
}
