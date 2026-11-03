package goextractor

import (
	"slices"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A function literal bound to a name inside a function is listed on it; an unbound one is not.
func TestNestedFuncLitsBoundToANameAreListed(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"s/s.go": `package s

func Send() int {
	onClose := func() int {
		return 1
	}
	var onOpen = func() int { return 2 }
	_ = func() {}
	go func() {}()
	return onClose() + onOpen()
}
`,
	})
	for i := range ff {
		if ff[i].Kind == facts.KindSymbol && ff[i].Name == "s.Send" {
			var got []string
			for _, n := range facts.NestedOf(&ff[i]) {
				got = append(got, n.String())
			}
			if want := []string{"onClose:4-6", "onOpen:7-7"}; !slices.Equal(got, want) {
				t.Errorf("nested = %v, want %v", got, want)
			}
			return
		}
	}
	t.Fatal("no symbol s.Send")
}
