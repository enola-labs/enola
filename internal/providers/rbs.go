package providers

import (
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

type declaredContract struct {
	signatures map[string]bool
	files      map[string]bool
}

func LinkDeclaredContracts(store *facts.Store, startIdx int) int {
	window := store.FactsRef()[startIdx:]
	declared := map[string]*declaredContract{}
	for _, f := range window {
		if f.Kind != facts.KindSymbol || f.PropString(PropResolutionLevel) != LevelDeclared {
			continue
		}
		key := contractIdentity(f)
		file := f.PropString(PropDeclaredIn)
		if key == "" || file == "" {
			continue
		}
		c := declared[key]
		if c == nil {
			c = &declaredContract{signatures: map[string]bool{}, files: map[string]bool{}}
			declared[key] = c
		}
		if sig := f.PropString("signature"); sig != "" {
			c.signatures[sig] = true
		}
		c.files[file] = true
	}
	if len(declared) == 0 {
		return 0
	}

	annotated := 0
	store.UpdateRange(startIdx, func(f *facts.Fact) {
		if f.Kind != facts.KindSymbol || f.PropString(PropResolutionLevel) == LevelDeclared {
			return
		}
		c := declared[f.Name]
		if c == nil {
			return
		}
		if f.Props == nil {
			f.Props = map[string]any{}
		}
		f.SetProp(PropTyped, true)
		if sig := joinSortedSet(c.signatures, " | "); sig != "" {
			f.SetProp(PropDeclaredSignature, sig)
		}
		f.SetProp(PropDeclaredIn, mergeViaSet(f.PropString(PropDeclaredIn), c.files))
		annotated++
	})
	return annotated
}

func contractIdentity(f facts.Fact) string {
	receiver := f.PropString("receiver")
	method := f.PropString("method")
	if receiver == "" || method == "" {
		return ""
	}
	separator := "#"
	if singleton, _ := f.PropAny("singleton").(bool); singleton {
		separator = "."
	}
	return receiver + separator + method
}

func joinSortedSet(set map[string]bool, separator string) string {
	members := make([]string, 0, len(set))
	for member := range set {
		members = append(members, member)
	}
	sort.Strings(members)
	return strings.Join(members, separator)
}
