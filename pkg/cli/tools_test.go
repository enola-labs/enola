package cli

import (
	"strings"
	"testing"
)

func TestRenderToolList_ListsEveryTool(t *testing.T) {
	out := RenderToolList()

	for _, tool := range OSSTools() {
		if !strings.Contains(out, tool.Name) {
			t.Errorf("tool %q missing from the catalogue", tool.Name)
		}
	}
	// One group, so there is nothing to distinguish it from and nothing to label.
	for _, unwanted := range []string{"OSS tools:", "Enterprise", "ENOLA_LICENSE_KEY"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("catalogue should not mention %q:\n%s", unwanted, out)
		}
	}
}

// The longest name sets the column for every row, so no description runs into a name.
func TestRenderToolList_LongestNameSetsTheColumn(t *testing.T) {
	out := RenderToolList()

	widest := 0
	for _, tool := range OSSTools() {
		if len(tool.Name) > widest {
			widest = len(tool.Name)
		}
	}
	if widest < nameColumn {
		widest = nameColumn
	}

	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "  ") || strings.TrimSpace(line) == "" {
			continue
		}
		name, _, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		desc := strings.Index(line, strings.TrimSpace(line[2+len(name):]))
		if want := 2 + widest + 2; desc != want {
			t.Errorf("%q description starts at column %d, want %d", name, desc, want)
		}
	}
}
