package rubyextractor

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	sitter "github.com/enola-labs/enola/internal/treesitter"
	ruby "github.com/tree-sitter/tree-sitter-ruby/bindings/go"
)

// Route-file discovery.
//
// A Rails application's routes are not one file. They are the root config/routes.rb
// plus every file it pulls in — `draw(:pkg)` delegations, and the config/routes.rb of
// every engine it mounts. Reading only the root file is how solidus (a monorepo of six
// mountable engines, no root config/ at all) reported ZERO Rails routes while declaring
// 195, and how discourse's 25 plugin route files and GitLab's 38 ee/config/routes files
// went unread.
//
// The rules below are deliberately shape-based rather than name-based: a route file is
// any `<root>/config/routes.rb` or any `.rb` under a `config/routes/` directory, at any
// depth. That over-collects by design — a file that is a route file but is never loaded
// contributes routes nobody serves, which is a smaller error than dropping a whole
// engine — with two exclusions for the shapes that are definitely not live routes.

// isRouteFile returns true if the file path looks like a Rails route file, anywhere in
// the repository: the application's own config/routes.rb, an engine's or plugin's, a
// config/routes/<pkg>.rb delegation target, or one nested below it.
func isRouteFile(relFile string) bool {
	slash := filepath.ToSlash(relFile)
	if !strings.HasSuffix(slash, ".rb") {
		return false
	}
	if isNonLiveRoutePath(slash) {
		return false
	}
	parts := strings.Split(slash, "/")
	// <anything>/config/routes.rb, or config/routes.rb at the root.
	if parts[len(parts)-1] == "routes.rb" {
		if len(parts) == 1 {
			return false // a bare routes.rb at the repo root is not a Rails route file
		}
		return parts[len(parts)-2] == "config"
	}
	// <anything>/config/routes/**/<name>.rb
	for i := 0; i+1 < len(parts)-1; i++ {
		if parts[i] == "config" && parts[i+1] == "routes" {
			return true
		}
	}
	return false
}

// isNonLiveRoutePath reports whether a path that otherwise looks like a route file is a
// generator template or a test harness fixture rather than routes the app serves.
// Solidus ships both — core/lib/generators/spree/dummy/templates/rails/routes.rb and
// core/lib/spree/testing_support/dummy_app/routes.rb — and neither is mounted anywhere.
func isNonLiveRoutePath(slash string) bool {
	for _, seg := range strings.Split(slash, "/") {
		switch seg {
		case "generators", "templates", "dummy_app", "fixtures", "testing_support":
			return true
		}
	}
	return false
}

// routeFileIndex classifies the route files found in a repository so extractAllRoutes
// can parse them in dependency order: the application root first (it declares the
// prefixes), then the files it delegates to or mounts.
type routeFileIndex struct {
	// appRoot is the application's own config/routes.rb ("config/routes.rb"), or "" for
	// an engine-only repository such as solidus.
	appRoot string
	// engineRoots maps an engine directory (the dir holding config/routes.rb, e.g.
	// "api" or "plugins/chat") to that route file.
	engineRoots map[string]string
	// drawTargets maps a draw key ("project", "remote_development/resources") to every
	// file that satisfies it. GitLab overrides `draw` to load BOTH config/routes/x.rb
	// and ee/config/routes/x.rb, so this is deliberately a list, keyed on the path tail
	// below config/routes/ rather than on the file's full path.
	drawTargets map[string][]string
	// all is every route file, so nothing discovered can be silently dropped.
	all []string
}

// indexRouteFiles classifies every route file in files.
func indexRouteFiles(files []string) routeFileIndex {
	idx := routeFileIndex{
		engineRoots: map[string]string{},
		drawTargets: map[string][]string{},
	}
	appRoot := filepath.Join("config", "routes.rb")
	for _, relFile := range files {
		if !isRubyFile(relFile) || !isRouteFile(relFile) {
			continue
		}
		idx.all = append(idx.all, relFile)
		slash := filepath.ToSlash(relFile)
		if relFile == appRoot {
			idx.appRoot = relFile
			continue
		}
		if strings.HasSuffix(slash, "/config/routes.rb") {
			idx.engineRoots[strings.TrimSuffix(slash, "/config/routes.rb")] = relFile
			continue
		}
		// A config/routes/** delegation target. The key is everything below the
		// config/routes/ segment, minus ".rb".
		if i := strings.Index(slash, "config/routes/"); i >= 0 {
			key := strings.TrimSuffix(slash[i+len("config/routes/"):], ".rb")
			idx.drawTargets[key] = append(idx.drawTargets[key], relFile)
		}
	}
	return idx
}

// --- engine constant resolution ---
//
// `mount Spree::Core::Engine, at: "/"` names a class, not a directory, and the routes
// it mounts live in that engine's own config/routes.rb. Resolving one to the other is
// what turns a mount into a URL prefix for a whole route file.

// engineConstants maps a Rails::Engine subclass constant ("Spree::Core::Engine",
// "DiscourseAi::Engine") to the engine directory that owns it, by reading the
// `<engineDir>/lib/**/engine.rb` files that sit beside each engine route file.
//
// The scan is bounded to engine directories that actually have a route file: an engine
// with no routes needs no mapping, and scanning every .rb in the repository for
// `< ::Rails::Engine` would cost a second full pass for nothing.
func engineConstants(repoPath string, idx routeFileIndex, files []string) map[string]string {
	// Group candidate engine.rb files by the engine directory that contains them.
	byDir := map[string][]string{}
	for _, relFile := range files {
		if filepath.Base(relFile) != "engine.rb" {
			continue
		}
		slash := filepath.ToSlash(relFile)
		for dir := range idx.engineRoots {
			if strings.HasPrefix(slash, dir+"/lib/") {
				byDir[dir] = append(byDir[dir], relFile)
			}
		}
	}

	// Iterate directories in sorted order: two engines declaring the same constant is
	// pathological but possible, and "first writer wins" over Go's randomized map order
	// would make the graph differ between runs.
	dirs := make([]string, 0, len(byDir))
	for dir := range byDir {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)

	out := map[string]string{}
	for _, dir := range dirs {
		for _, relFile := range byDir[dir] {
			src, err := os.ReadFile(filepath.Join(repoPath, relFile))
			if err != nil {
				continue
			}
			for _, c := range engineClassNames(src) {
				// First writer wins, so the mapping is independent of file order.
				if _, seen := out[c]; !seen {
					out[c] = dir
				}
			}
		}
	}
	return out
}

// engineClassNames returns the fully-qualified names of every `class X < ::Rails::Engine`
// (or `< Rails::Engine`) declared in src, qualified by its enclosing module nesting.
func engineClassNames(src []byte) []string {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(ruby.Language())); err != nil {
		return nil
	}
	tree := parser.Parse(src, nil)
	if tree == nil {
		return nil
	}
	defer tree.Close()

	var out []string
	var walk func(n *sitter.Node, nesting []string)
	walk = func(n *sitter.Node, nesting []string) {
		if n == nil {
			return
		}
		switch kindOf(n) {
		case "module":
			name := rubyText(n.ChildByFieldName("name"), src)
			if name != "" {
				nesting = append(append([]string{}, nesting...), name)
			}
		case "class":
			name := rubyText(n.ChildByFieldName("name"), src)
			super := strings.TrimPrefix(rubyText(n.ChildByFieldName("superclass"), src), "<")
			super = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(super), "::"))
			if name != "" {
				qualified := strings.Join(append(append([]string{}, nesting...), name), "::")
				if super == "Rails::Engine" {
					out = append(out, qualified)
				}
				nesting = append(append([]string{}, nesting...), name)
			}
		}
		for i := uint(0); i < n.ChildCount(); i++ {
			walk(n.Child(i), nesting)
		}
	}
	walk(tree.RootNode(), nil)
	return out
}

// normalizeConstant strips a leading "::" so `::API::API` and `API::API` compare equal.
func normalizeConstant(s string) string {
	return strings.TrimPrefix(strings.TrimSpace(s), "::")
}
