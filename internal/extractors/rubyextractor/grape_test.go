package rubyextractor

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func grapeIndex(ff []facts.Fact) map[string]facts.Fact {
	out := map[string]facts.Fact{}
	for _, f := range ff {
		m, _ := f.Props["method"].(string)
		out[m+" "+f.Name] = f
	}
	return out
}

func grapeNames(m map[string]facts.Fact) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, "\n  ")
}

func TestParseGrapeFile_NestingConstructs(t *testing.T) {
	src := `
module API
  class Lint < ::API::Base
    resource :projects do
      desc 'Validate config' do
        detail 'ignored'
      end
      params do
        requires :id, type: String
      end
      get ':id/ci/lint' do
        present result
      end

      route_param :id do
        namespace 'members' do
          post do
            create_member
          end
          delete ':user_id' do
            destroy_member
          end
        end
      end
    end
  end
end
`
	classes := parseGrapeFile([]byte(src), "lib/api/lint.rb")
	if len(classes) != 1 {
		t.Fatalf("want 1 grape class, got %d: %+v", len(classes), classes)
	}
	c := classes[0]
	if c.name != "API::Lint" {
		t.Errorf("name = %q, want API::Lint", c.name)
	}
	got := map[string]bool{}
	for _, r := range c.routes {
		got[r.method+" "+r.path] = true
	}
	for _, want := range []string{
		"GET /projects/:id/ci/lint",
		"POST /projects/:id/members",
		"DELETE /projects/:id/members/:user_id",
	} {
		if !got[want] {
			t.Errorf("missing %q; got %v", want, got)
		}
	}
	if len(got) != 3 {
		t.Errorf("unexpected extra routes: %v", got)
	}
}

// `params do ... end` and `desc do ... end` carry no routes; descending into them would
// invent endpoints out of validation DSL.
func TestParseGrapeFile_MetadataBlocksAreNotRoutes(t *testing.T) {
	src := `
module API
  class Thing < ::API::Base
    params do
      optional :get, type: String
    end
    get 'thing' do
      1
    end
  end
end
`
	classes := parseGrapeFile([]byte(src), "lib/api/thing.rb")
	if len(classes) != 1 || len(classes[0].routes) != 1 {
		t.Fatalf("want exactly 1 route, got %+v", classes)
	}
	if classes[0].routes[0].path != "/thing" {
		t.Errorf("path = %q", classes[0].routes[0].path)
	}
}

func TestGrapeAPIFiles_TransitiveInheritance(t *testing.T) {
	// The GitLab shape: exactly one class inherits Grape directly, everything else
	// inherits that. A one-level check would find one file out of a thousand.
	cls := func(name, super, file string) facts.Fact {
		return facts.Fact{
			Kind: facts.KindSymbol, Name: name, File: file,
			Props: map[string]any{"symbol_kind": facts.SymbolClass, "superclass": super},
		}
	}
	got := grapeAPIFiles([]facts.Fact{
		cls("API::Base", "Grape::API::Instance", "lib/api/base.rb"),
		cls("API::API", "::API::Base", "lib/api/api.rb"),
		cls("API::Projects", "::API::Base", "lib/api/projects.rb"),
		cls("API::Ci::Runner", "API::Base", "lib/api/ci/runner.rb"),
		// Not Grape.
		cls("User", "ApplicationRecord", "app/models/user.rb"),
		cls("PostsController", "ApplicationController", "app/controllers/posts_controller.rb"),
	})
	want := []string{"lib/api/api.rb", "lib/api/base.rb", "lib/api/ci/runner.rb", "lib/api/projects.rb"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("grapeAPIFiles = %v, want %v", got, want)
	}
}

// TestExtractGrapeRoutes_MountComposesPrefix is the GitLab v4 API end to end: a root
// class carrying `prefix`/`version` mounts leaf classes, and each leaf's routes are
// served below the composed prefix — which lives in a different file from the routes.
func TestExtractGrapeRoutes_MountComposesPrefix(t *testing.T) {
	repo := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("lib/api/base.rb", "module API\n  class Base < Grape::API::Instance\n  end\nend\n")
	write("lib/api/api.rb", `
module API
  class API < ::API::Base
    prefix :api
    version 'v4', using: :path
    mount ::API::Projects
    namespace 'internal' do
      mount ::API::Internal::Base
    end
  end
end
`)
	write("lib/api/projects.rb", `
module API
  class Projects < ::API::Base
    resource :projects do
      get do
        list
      end
      route_param :id do
        delete do
          destroy
        end
      end
    end
  end
end
`)
	write("lib/api/internal/base.rb", `
module API
  module Internal
    class Base < ::API::Base
      get '/check' do
        ok
      end
    end
  end
end
`)

	classFacts := []facts.Fact{
		{Kind: facts.KindSymbol, Name: "API::Base", File: "lib/api/base.rb",
			Props: map[string]any{"symbol_kind": facts.SymbolClass, "superclass": "Grape::API::Instance"}},
		{Kind: facts.KindSymbol, Name: "API::API", File: "lib/api/api.rb",
			Props: map[string]any{"symbol_kind": facts.SymbolClass, "superclass": "::API::Base"}},
		{Kind: facts.KindSymbol, Name: "API::Projects", File: "lib/api/projects.rb",
			Props: map[string]any{"symbol_kind": facts.SymbolClass, "superclass": "::API::Base"}},
		{Kind: facts.KindSymbol, Name: "API::Internal::Base", File: "lib/api/internal/base.rb",
			Props: map[string]any{"symbol_kind": facts.SymbolClass, "superclass": "::API::Base"}},
	}

	idx := grapeIndex(extractGrapeRoutes(context.Background(), repo, classFacts))
	for _, want := range []string{
		"GET /api/v4/projects",
		"DELETE /api/v4/projects/:id",
		"GET /api/v4/internal/check",
	} {
		if _, ok := idx[want]; !ok {
			t.Errorf("missing %q; got:\n  %s", want, grapeNames(idx))
		}
	}
	if len(idx) != 3 {
		t.Errorf("unexpected extra routes:\n  %s", grapeNames(idx))
	}
	f := idx["GET /api/v4/projects"]
	if f.Props["framework"] != "grape" {
		t.Errorf("framework = %v, want grape", f.Props["framework"])
	}
	if handledBy(f) != "API::Projects" {
		t.Errorf("handled_by = %q, want API::Projects", handledBy(f))
	}
}

// A repository with no Grape at all must cost nothing and produce nothing.
func TestExtractGrapeRoutes_NoGrapeIsANoOp(t *testing.T) {
	got := extractGrapeRoutes(context.Background(), t.TempDir(), []facts.Fact{
		{Kind: facts.KindSymbol, Name: "User", File: "app/models/user.rb",
			Props: map[string]any{"symbol_kind": facts.SymbolClass, "superclass": "ApplicationRecord"}},
	})
	if len(got) != 0 {
		t.Errorf("want no facts, got %+v", got)
	}
}

// `version ... using: :header` and `using: :param` do not change the URL.
func TestGrapeVersionUsingHeaderDoesNotPrefix(t *testing.T) {
	classes := parseGrapeFile([]byte(`
module API
  class V < Grape::API
    version 'v2', using: :header, vendor: 'acme'
    get 'ping' do
      1
    end
  end
end
`), "lib/api/v.rb")
	if len(classes) != 1 {
		t.Fatalf("got %+v", classes)
	}
	if classes[0].prefix != "" {
		t.Errorf("prefix = %q, want empty for a header-versioned API", classes[0].prefix)
	}
	if len(classes[0].routes) != 1 || classes[0].routes[0].path != "/ping" {
		t.Errorf("routes = %+v", classes[0].routes)
	}
}

// A mount cycle must terminate rather than recurse forever.
func TestExtractGrapeRoutes_MountCycleTerminates(t *testing.T) {
	repo := t.TempDir()
	for rel, body := range map[string]string{
		"lib/a.rb": "class A < Grape::API\n  mount B\n  get 'a' do\n    1\n  end\nend\n",
		"lib/b.rb": "class B < Grape::API\n  mount A\n  get 'b' do\n    1\n  end\nend\n",
	} {
		if err := os.MkdirAll(filepath.Join(repo, "lib"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, filepath.FromSlash(rel)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := extractGrapeRoutes(context.Background(), repo, []facts.Fact{
		{Kind: facts.KindSymbol, Name: "A", File: "lib/a.rb",
			Props: map[string]any{"symbol_kind": facts.SymbolClass, "superclass": "Grape::API"}},
		{Kind: facts.KindSymbol, Name: "B", File: "lib/b.rb",
			Props: map[string]any{"symbol_kind": facts.SymbolClass, "superclass": "Grape::API"}},
	})
	// Both classes are mounted by the other, so neither is a root and nothing is
	// emitted — but the important assertion is that this returns at all.
	for _, f := range got {
		if f.Name == "" {
			t.Errorf("empty route name in %+v", f)
		}
	}
}

// A route whose path is built at runtime in part, by its own interpolated path or
// by an enclosing block or mount under a computed one, says so; a literal one, a
// symbol and a route_param do not.
func TestExtractGrapeRoutes_PathBuiltAtRuntimeIsPartial(t *testing.T) {
	repo := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(repo, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("lib/api/notes.rb", `
module API
  class Notes < Grape::API::Instance
    resource :projects do
      get ":id/#{noteables_path}/notes" do
      end
      route_param :id do
        get 'plain' do
        end
      end
    end
    resource parent_type.pluralize.to_sym, requirements: {} do
      get 'emoji' do
      end
      mount ::API::Inner
    end
  end
end
`)
	write("lib/api/inner.rb", `
module API
  class Inner < Grape::API::Instance
    get 'inner' do
    end
  end
end
`)
	classFacts := []facts.Fact{
		{Kind: facts.KindSymbol, Name: "API::Notes", File: "lib/api/notes.rb",
			Props: map[string]any{"symbol_kind": facts.SymbolClass, "superclass": "Grape::API::Instance"}},
		{Kind: facts.KindSymbol, Name: "API::Inner", File: "lib/api/inner.rb",
			Props: map[string]any{"symbol_kind": facts.SymbolClass, "superclass": "Grape::API::Instance"}},
	}
	want := map[string]bool{
		"GET /projects/:id":       true,
		"GET /projects/:id/plain": false,
		"GET /emoji":              true,
		"GET /inner":              true,
	}
	idx := grapeIndex(extractGrapeRoutes(context.Background(), repo, classFacts))
	for name, partial := range want {
		f, ok := idx[name]
		if !ok {
			t.Errorf("missing %q; got:\n  %s", name, grapeNames(idx))
			continue
		}
		if got := f.PropBool(facts.PropPathPartial); got != partial {
			t.Errorf("%s: path_partial = %v, want %v", name, got, partial)
		}
	}
}
