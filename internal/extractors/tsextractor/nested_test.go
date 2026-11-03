package tsextractor

import (
	"slices"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A named function inside another that is not a symbol is listed on the innermost symbol
// holding it; an anonymous one is not, and neither is one that is a symbol already.
func TestNestedFunctionsWithoutSymbolsAreListed(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/client.ts": `export class Client {
  send(data: string) {
    function onClose() {
      return 1;
    }
    const onOpen = () => {
      const inner = function () { return 2; };
      return inner;
    };
    [1].map((x) => x + 1);
    const handlers = { onError() { return 3; }, onData: () => 4 };
    return { onClose, onOpen, handlers };
  }
}

export function plugin() {
  return { transform() { return 5; } };
}
`,
	}, false)
	nested := map[string][]string{}
	for i := range ff {
		if ff[i].Kind == facts.KindSymbol {
			for _, n := range facts.NestedOf(&ff[i]) {
				nested[ff[i].Name] = append(nested[ff[i].Name], n.String())
			}
		}
	}
	want := []string{"onClose:3-5", "onOpen:6-9", "inner:7-7", "onError:11-11", "onData:11-11"}
	got := nested["src.Client.send"]
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("Client.send nested = %v, want %v", got, want)
	}
	if n := nested["src.plugin"]; len(n) != 0 {
		t.Errorf("plugin lists %v: its returned transform is a symbol of its own", n)
	}
}
