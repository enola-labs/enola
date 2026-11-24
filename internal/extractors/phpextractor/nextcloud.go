package phpextractor

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/enola-labs/enola/internal/treesitter"
	php "github.com/tree-sitter/tree-sitter-php/bindings/go"
)

// A Nextcloud app is wired by files the server reads, not by calls: appinfo/info.xml
// names the app and registers its background jobs, settings, providers and commands
// by class name; appinfo/routes.php maps each URL to `controller#action`; and a
// controller action carries its own route as an #[ApiRoute] or #[FrontpageRoute]
// attribute. Without them every controller, job and provider reads as dead.
//
// An app is a directory holding appinfo/info.xml. Its routes are served under
// /apps/<id> (web) and /ocs/v2.php/apps/<id> (OCS), and its controllers live in
// OCA\<namespace>\Controller, the namespace info.xml states or the capitalized id.

const (
	ncAttrNamespace = `OCP\AppFramework\Http\Attribute\`
	ncWebPrefix     = "/apps/"
	ncOCSPrefix     = "/ocs/v2.php/apps/"
)

// propNextcloudRoutes carries, on a controller action, the routes its attributes
// declare (`ocs|POST|/url|root`), from the per-file walk to emitNextcloudRoutes; it
// never reaches the graph.
const propNextcloudRoutes = "_nextcloud_routes"

type ncApp struct {
	root      string // the app's directory, "" at the repository root
	id        string
	namespace string
}

var (
	ncIDTag        = regexp.MustCompile(`<id>\s*([\w.-]+)\s*</id>`)
	ncNamespaceTag = regexp.MustCompile(`<namespace>\s*([\w\\]+)\s*</namespace>`)
	// ncClassText matches an element whose whole text is a class name.
	ncClassText = regexp.MustCompile(`>\s*\\?([A-Za-z_]\w*(?:\\[A-Za-z_]\w*)+)\s*</`)
)

// isNextcloudInfo reports an app manifest: appinfo/info.xml.
func isNextcloudInfo(rel string) bool {
	return rel == "appinfo/info.xml" || strings.HasSuffix(rel, "/appinfo/info.xml")
}

// nextcloudApps reads every app manifest among files. It also returns, per
// manifest, a file reference naming each declared class the manifest registers.
func nextcloudApps(repoPath string, files []string, declared map[string]bool) ([]ncApp, []facts.Fact) {
	var apps []ncApp
	var refs []facts.Fact
	for _, rel := range files {
		if !isNextcloudInfo(rel) || facts.IsTestPath(rel) {
			continue
		}
		src, err := os.ReadFile(filepath.Join(repoPath, rel))
		if err != nil {
			continue
		}
		m := ncIDTag.FindSubmatch(src)
		if m == nil {
			continue
		}
		app := ncApp{root: strings.TrimSuffix(strings.TrimSuffix(rel, "appinfo/info.xml"), "/"), id: string(m[1])}
		if ns := ncNamespaceTag.FindSubmatch(src); ns != nil {
			app.namespace = string(ns[1])
		} else {
			app.namespace = strings.ToUpper(app.id[:1]) + app.id[1:]
		}
		apps = append(apps, app)
		if ref, ok := infoXMLRef(rel, src, declared); ok {
			refs = append(refs, ref)
		}
	}
	return apps, refs
}

func infoXMLRef(rel string, src []byte, declared map[string]bool) (facts.Fact, bool) {
	found := map[string]bool{}
	for _, m := range ncClassText.FindAllSubmatch(src, -1) {
		if class := string(m[1]); declared[class] {
			found[class] = true
		}
	}
	if len(found) == 0 {
		return facts.Fact{}, false
	}
	classes := make([]string, 0, len(found))
	for c := range found {
		classes = append(classes, c)
	}
	sort.Strings(classes)
	rels := make([]facts.Relation, 0, len(classes))
	for _, c := range classes {
		rels = append(rels, facts.Relation{Kind: facts.RelNames, Target: c})
	}
	return facts.Fact{Kind: facts.KindFileRef, Name: rel, File: rel, Line: 1,
		Props: map[string]any{"language": "php"}, Relations: rels}, true
}

// appOf returns the app a file belongs to: the one whose directory holds it most
// closely.
func appOf(apps []ncApp, rel string) (ncApp, bool) {
	best, found := ncApp{}, false
	for _, a := range apps {
		if a.root != "" && !strings.HasPrefix(rel, a.root+"/") {
			continue
		}
		if !found || len(a.root) > len(best.root) {
			best, found = a, true
		}
	}
	return best, found
}

// ncRoute is one route an app declares, before its handler is resolved.
type ncRoute struct {
	ocs     bool
	verb    string
	url     string
	root    string // replaces /apps/<id> when the route sets one
	handler string
	file    string
	line    int
}

func (r ncRoute) path(app ncApp) string {
	prefix := ncWebPrefix + app.id
	if r.root != "" {
		prefix = r.root
	}
	if r.ocs {
		prefix = strings.TrimSuffix(ncOCSPrefix, "/apps/") + prefix
	}
	segs := append(splitSegs(prefix), splitSegs(r.url)...)
	return "/" + strings.Join(segs, "/")
}

// ncCamel is Nextcloud's reading of a route name part: `social_api` is SocialApi
// as a controller (first) and socialApi as an action.
func ncCamel(s string, first bool) string {
	parts := strings.Split(s, "_")
	for i, p := range parts {
		if p != "" && (i > 0 || first) {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, "")
}

func (a ncApp) action(controller, action string) string {
	return `OCA\` + a.namespace + `\Controller\` + ncCamel(controller, true) + "Controller::" + ncCamel(action, false)
}

// readRoutesFile returns the routes an app's appinfo/routes.php declares: each
// `['name' => 'c#a', 'url' => ..., 'verb' => ...]` under `routes` (web) or `ocs`,
// and each `'name' => ['url' => ...]` under `resources` or `ocs-resources`, which
// stands for the five REST actions.
func readRoutesFile(src []byte, rel string, app ncApp) []ncRoute {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(php.LanguagePHP())); err != nil {
		return nil
	}
	tree := parser.Parse(src, nil)
	defer tree.Close()
	var out []ncRoute
	var walk func(n *sitter.Node, section string)
	walk = func(n *sitter.Node, section string) {
		if n == nil {
			return
		}
		if kindOf(n) == "array_element_initializer" {
			key, val := arrayPair(n, src)
			switch key {
			case "routes", "ocs", "resources", "ocs-resources":
				walk(val, key)
				return
			}
			if (section == "resources" || section == "ocs-resources") && kindOf(val) == "array_creation_expression" {
				if url := arrayStrings(val, src)["url"]; url != "" && key != "" {
					out = append(out, resourceRoutes(app, key, url, section == "ocs-resources", rel, line(n))...)
					return
				}
			}
		}
		if kindOf(n) == "array_creation_expression" && (section == "routes" || section == "ocs") {
			kv := arrayStrings(n, src)
			if controller, action, ok := strings.Cut(kv["name"], "#"); ok && kv["url"] != "" {
				verb := strings.ToUpper(kv["verb"])
				if verb == "" {
					verb = "GET"
				}
				out = append(out, ncRoute{ocs: section == "ocs", verb: verb, url: kv["url"], root: kv["root"],
					handler: app.action(controller, action), file: rel, line: line(n)})
				return
			}
		}
		for i := uint(0); i < n.NamedChildCount(); i++ {
			walk(n.NamedChild(i), section)
		}
	}
	walk(tree.RootNode(), "")
	return out
}

func resourceRoutes(app ncApp, name, url string, ocs bool, rel string, ln int) []ncRoute {
	id := strings.TrimSuffix(url, "/") + "/{id}"
	var out []ncRoute
	for _, r := range []struct{ verb, url, action string }{
		{"GET", url, "index"}, {"GET", id, "show"}, {"POST", url, "create"}, {"PUT", id, "update"}, {"DELETE", id, "destroy"},
	} {
		out = append(out, ncRoute{ocs: ocs, verb: r.verb, url: r.url, handler: app.action(name, r.action), file: rel, line: ln})
	}
	return out
}

// arrayPair returns the string key and the value of one `key => value` element.
func arrayPair(n *sitter.Node, src []byte) (string, *sitter.Node) {
	if n.NamedChildCount() != 2 {
		return "", nil
	}
	return stringLiteral(n.NamedChild(0), src), n.NamedChild(1)
}

// arrayStrings returns the string-keyed string values of an array literal.
func arrayStrings(n *sitter.Node, src []byte) map[string]string {
	out := map[string]string{}
	for i := uint(0); i < n.NamedChildCount(); i++ {
		el := n.NamedChild(i)
		if kindOf(el) != "array_element_initializer" {
			continue
		}
		if key, val := arrayPair(el, src); key != "" {
			out[key] = stringLiteral(val, src)
		}
	}
	return out
}

// noteNextcloudRouteAttributes records the #[ApiRoute] and #[FrontpageRoute]
// attributes of a method, by the framework type each resolves to.
func (w *phpWalker) noteNextcloudRouteAttributes(ownerIdx int, attrs *sitter.Node) {
	if attrs == nil {
		return
	}
	var scan func(n *sitter.Node)
	scan = func(n *sitter.Node) {
		if kindOf(n) != "attribute" {
			for i := uint(0); i < n.NamedChildCount(); i++ {
				scan(n.NamedChild(i))
			}
			return
		}
		var kind string
		for i := uint(0); i < n.NamedChildCount(); i++ {
			c := n.NamedChild(i)
			if k := kindOf(c); k != "name" && k != "qualified_name" {
				continue
			}
			switch w.className(phpText(c, w.src)) {
			case ncAttrNamespace + "ApiRoute":
				kind = "ocs"
			case ncAttrNamespace + "FrontpageRoute":
				kind = "web"
			}
			break
		}
		if kind == "" {
			return
		}
		args := map[string]string{}
		if params := n.ChildByFieldName("parameters"); params != nil {
			positional := []string{"verb", "url"}
			for i := uint(0); i < params.NamedChildCount(); i++ {
				a := params.NamedChild(i)
				if kindOf(a) != "argument" {
					continue
				}
				if name := a.ChildByFieldName("name"); name != nil {
					args[phpText(name, w.src)] = stringLiteral(argValue(a), w.src)
				} else if len(positional) > 0 {
					args[positional[0]] = stringLiteral(argValue(a), w.src)
					positional = positional[1:]
				}
			}
		}
		if args["verb"] == "" || args["url"] == "" {
			return
		}
		w.out[ownerIdx].AddPropStrings(propNextcloudRoutes,
			strings.Join([]string{kind, strings.ToUpper(args["verb"]), args["url"], args["root"]}, "|"))
	}
	scan(attrs)
}

// nextcloudFacts returns the route facts and manifest references of the Nextcloud
// apps in files, and turns the route attributes noted on actions into routes.
func nextcloudFacts(repoPath string, files []string, allFacts []facts.Fact) []facts.Fact {
	apps, out := nextcloudApps(repoPath, files, declaredTypes(allFacts))
	var routes []ncRoute
	for i := range allFacts {
		f := &allFacts[i]
		raw, ok := f.Props[propNextcloudRoutes]
		if !ok {
			continue
		}
		delete(f.Props, propNextcloudRoutes)
		if _, ok := appOf(apps, f.File); !ok {
			continue
		}
		notes, _ := raw.([]string)
		for _, note := range notes {
			p := strings.SplitN(note, "|", 4)
			if len(p) != 4 {
				continue
			}
			routes = append(routes, ncRoute{ocs: p[0] == "ocs", verb: p[1], url: p[2], root: p[3],
				handler: f.Name, file: f.File, line: f.Line})
		}
	}
	routeFiles := map[string]ncApp{}
	for _, a := range apps {
		routeFiles[path.Join(a.root, "appinfo/routes.php")] = a
	}
	for _, rel := range files {
		app, ok := routeFiles[rel]
		if !ok {
			continue
		}
		src, err := os.ReadFile(filepath.Join(repoPath, rel))
		if err != nil {
			continue
		}
		routes = append(routes, readRoutesFile(src, rel, app)...)
	}
	if len(routes) == 0 {
		return out
	}
	methods := map[string]string{} // lower-case Class::method -> the declared name
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind == facts.KindSymbol && f.PropString("symbol_kind") == facts.SymbolMethod {
			methods[strings.ToLower(f.Name)] = f.Name
		}
	}
	for _, r := range routes {
		app, _ := appOf(apps, r.file)
		p := r.path(app)
		props := map[string]any{
			facts.PropRole: facts.RoleServer,
			"method":       r.verb,
			"framework":    "nextcloud",
			"language":     "php",
			"path":         p,
			"handler":      r.handler,
		}
		rels := []facts.Relation{{Kind: facts.RelDeclares, Target: factpath.Dir(r.file)}}
		// PHP resolves a class and a method without regard to case.
		if target, ok := methods[strings.ToLower(r.handler)]; ok {
			props["handler"] = target
			rels = append(rels, facts.Relation{Kind: facts.RelHandledBy, Target: target})
		}
		out = append(out, facts.Fact{Kind: facts.KindRoute, Name: p, File: r.file, Line: r.line, Props: props, Relations: rels})
	}
	return out
}

// declaredTypes is the set of class-like types the PHP facts declare.
func declaredTypes(ff []facts.Fact) map[string]bool {
	out := map[string]bool{}
	for i := range ff {
		f := &ff[i]
		if f.Kind != facts.KindSymbol {
			continue
		}
		switch f.PropString("symbol_kind") {
		case facts.SymbolClass, facts.SymbolInterface, facts.SymbolEnum:
			out[f.Name] = true
		}
	}
	return out
}
