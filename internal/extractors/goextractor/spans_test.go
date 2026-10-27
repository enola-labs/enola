package goextractor

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A symbol's span is what maps a changed line or a reported location to the
// declaration it sits in. Each declaration form ends on the line its last token is on.
func TestSymbolSpans(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"pkg/spans.go": `package pkg

import "fmt"

// Alpha says hello.
func Alpha(a int) int {
	fmt.Println(a)
	return a
}

type Gamma struct {
	ID   string
	Size int
}

func (g *Gamma) Run() {
	Alpha(1)
}

type Delta interface {
	Open() error
	Close(
		force bool,
	) error
}

var Handler = func() {
	Alpha(2)
}

const (
	One = 1
	Two = 2
)
`,
	})

	want := map[string][2]int{
		"pkg.Alpha":       {6, 9},
		"pkg.Gamma":       {11, 14},
		"pkg.Gamma.Run":   {16, 18},
		"pkg.Delta":       {20, 25},
		"pkg.Delta.Open":  {21, 21},
		"pkg.Delta.Close": {22, 24},
		"pkg.Handler":     {27, 29},
		"pkg.One":         {32, 32},
		"pkg.Two":         {33, 33},
	}
	got := map[string][2]int{}
	for _, f := range ff {
		if f.Kind != facts.KindSymbol {
			continue
		}
		if f.EndLine < f.Line || f.Line <= 0 {
			t.Errorf("%s (%s): span %d-%d", f.Name, f.File, f.Line, f.EndLine)
		}
		for name := range want {
			if f.Name == name || strings.HasSuffix(f.Name, "/"+name) {
				got[name] = [2]int{f.Line, f.EndLine}
			}
		}
	}
	for name, span := range want {
		if got[name] != span {
			t.Errorf("%s: span %v, want %v", name, got[name], span)
		}
	}
}
