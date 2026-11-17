package tsextractor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A handler exported under an HTTP-method alias is the route's handler, and
// generateStaticParams is a root Next.js calls; an ordinary helper is neither.
func TestNextConventionExports(t *testing.T) {
	dir := t.TempDir()
	for path, body := range map[string]string{
		"package.json": `{"dependencies": {"next": "15.0.0", "react": "19.0.0"}}`,
		"app/api/[...slug]/route.ts": `function notFound() {
  return Response.json({ error: 'Not Found' }, { status: 404 })
}

export { notFound as GET, notFound as POST }
`,
		"app/docs/[slug]/page.tsx": `export async function generateStaticParams() {
  return [{ slug: 'a' }]
}

function helper() { return 1 }

export default function Page() { return <div>{helper()}</div> }
`,
	} {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := map[string]facts.Fact{}
	for _, f := range extractDir(t, dir) {
		if f.Kind == facts.KindSymbol {
			got[f.Name[len(f.Name)-len(lastDot(f.Name)):]] = f
		}
	}
	if f := got["notFound"]; f.PropString("web_component") != "route_handler" {
		t.Errorf("aliased handler web_component = %q, want route_handler", f.PropString("web_component"))
	}
	if f := got["generateStaticParams"]; f.PropString(facts.PropFrameworkRoot) != "nextjs" {
		t.Errorf("generateStaticParams framework_root = %q, want nextjs", f.PropString(facts.PropFrameworkRoot))
	}
	if f := got["helper"]; f.PropString(facts.PropFrameworkRoot) != "" || f.PropString("web_component") == "route_handler" {
		t.Errorf("an ordinary helper was marked: %v", f.Props)
	}
}

func lastDot(name string) string {
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == '.' {
			return name[i+1:]
		}
	}
	return name
}
