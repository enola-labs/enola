package providers

import (
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

func LinkRuntimeObservations(store *facts.Store, startIdx int) int {
	window := store.FactsRef()[startIdx:]
	observed := map[string]map[string]bool{}
	for _, f := range window {
		if f.Kind != facts.KindRoute || f.PropString(PropResolutionLevel) != LevelRuntimeObserved {
			continue
		}
		via := f.PropString(PropObservedVia)
		key := routeIdentity(f)
		if via == "" || key == "" {
			continue
		}
		if observed[key] == nil {
			observed[key] = map[string]bool{}
		}
		observed[key][via] = true
	}
	if len(observed) == 0 {
		return 0
	}

	annotated := 0
	store.UpdateRange(startIdx, func(f *facts.Fact) {
		if f.Kind != facts.KindRoute || f.PropString(PropResolutionLevel) == LevelRuntimeObserved {
			return
		}
		if f.PropString("role") == "client" {
			return
		}
		vias := observed[routeIdentity(*f)]
		if len(vias) == 0 {
			return
		}
		if f.Props == nil {
			f.Props = map[string]any{}
		}
		f.SetProp(PropRuntimeObserved, true)
		f.SetProp(PropObservedVia, mergeViaSet(f.PropString(PropObservedVia), vias))
		annotated++
	})
	return annotated
}

func routeIdentity(f facts.Fact) string {
	method := f.PropString("method")
	path := f.PropString("path")
	if path == "" {
		path = f.Name
	}
	if method == "" || path == "" {
		return ""
	}
	return method + "\x00" + path
}

func mergeViaSet(existing string, vias map[string]bool) string {
	set := map[string]bool{}
	for _, via := range strings.Fields(existing) {
		set[via] = true
	}
	for via := range vias {
		set[via] = true
	}
	merged := make([]string, 0, len(set))
	for via := range set {
		merged = append(merged, via)
	}
	sort.Strings(merged)
	return strings.Join(merged, " ")
}
