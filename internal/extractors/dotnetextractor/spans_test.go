package dotnetextractor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A symbol's span is what maps a changed line or a reported location to the
// declaration it sits in. Each declaration form ends on the line its last token is on;
// a Razor component and a XAML view stand for their whole file.
func TestSymbolSpans(t *testing.T) {
	files := map[string]string{
		"src/Shop/Shop.cs": `namespace Acme.Shop;

public class Shop
{
    public const string Name = "shop";

    public int Size { get; set; }

    public void Run()
    {
        System.Console.WriteLine(Name);
    }
}

public interface IStore
{
    void Open();
}
`,
		"src/Api/Program.cs": `var app = WebApplication.Create();

app.MapGet("/orders", () =>
{
    return Results.Ok();
});

app.Run();
`,
		"src/Web/Counter.razor": `<h1>Counter</h1>

<p>@count</p>

@code {
    private int count = 0;
}
`,
		"src/Desk/MainWindow.xaml": `<Window x:Class="Acme.Desk.MainWindow"
        xmlns="http://schemas.microsoft.com/winfx/2006/xaml/presentation"
        xmlns:x="http://schemas.microsoft.com/winfx/2006/xaml">
    <Grid />
</Window>
`,
	}
	dir := t.TempDir()
	var rels []string
	for rel, content := range files {
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		rels = append(rels, rel)
	}
	ff, err := New().Extract(context.Background(), dir, rels)
	if err != nil {
		t.Fatal(err)
	}
	checkSpans(t, ff, map[string][2]int{
		"Shop":                  {3, 13},
		"Shop.Name":             {5, 5},
		"Shop.Size":             {7, 7},
		"Shop.Run":              {9, 12},
		"IStore":                {15, 18},
		"IStore.Open":           {17, 17},
		"Counter":               {1, 7},
		"MainWindow":            {1, 5},
		"Program.handler:GET:3": {3, 6},
	})
}

// checkSpans asserts each wanted symbol (matched by the end of its name) has the span,
// and every symbol has a well-formed one.
func checkSpans(t *testing.T, ff []facts.Fact, want map[string][2]int) {
	t.Helper()
	got := map[string][2]int{}
	for _, f := range ff {
		if f.Kind != facts.KindSymbol {
			continue
		}
		t.Logf("%s %d-%d", f.Name, f.Line, f.EndLine)
		if f.EndLine < f.Line || f.Line <= 0 {
			t.Errorf("%s (%s): span %d-%d", f.Name, f.File, f.Line, f.EndLine)
		}
		for name := range want {
			if f.Name == name || strings.HasSuffix(f.Name, "."+name) || strings.HasSuffix(f.Name, "/"+name) {
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
