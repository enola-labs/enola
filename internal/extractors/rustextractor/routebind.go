package rustextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
)

// Route-to-handler binding.
//
// An axum route names its handler as the registration writes it: `list_users`, or
// `handlers::users::list`. A utoipa route names the function its attribute sits on.
// Neither is a symbol name, which is the directory of the declaring file and the
// function's own name, so no route carried a handled_by relation.
//
// The name is resolved against the functions the repository declares:
//
//   - A bare name is the function of that name in the route's own file. When the
//     file declares none, the function was imported, and it is the only one of that
//     name in the route's crate.
//   - A path is the function of that name in the module the path ends in:
//     `github::secret_scanning::verify` is `verify` in github/secret_scanning.rs or
//     github/secret_scanning/mod.rs. The route's own crate is searched first.
//
// A name that fits no function, or more than one, binds nothing.

// rustFunction is one declared function: its symbol name and the file declaring it.
type rustFunction struct {
	name string
	file string
}

// bindRustRouteHandlers adds handled_by to each Rust route whose handler resolves to
// one declared function.
func bindRustRouteHandlers(allFacts []facts.Fact, crates []crateInfo) {
	var byShort map[string][]rustFunction
	for i := range allFacts {
		f := &allFacts[i]
		if f.Kind != facts.KindRoute || f.PropString("language") != "rust" {
			continue
		}
		handler := f.PropString("handler")
		if handler == "" {
			continue
		}
		if byShort == nil {
			byShort = map[string][]rustFunction{}
			for j := range allFacts {
				s := &allFacts[j]
				if s.Kind == facts.KindSymbol && s.PropString("language") == "rust" && s.PropString("symbol_kind") == facts.SymbolFunc {
					short := facts.ShortName(s.Name)
					byShort[short] = append(byShort[short], rustFunction{name: s.Name, file: s.File})
				}
			}
		}
		if target := resolveRustHandler(handler, f.File, byShort, crates); target != "" {
			f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelHandledBy, Target: target})
		}
	}
}

// resolveRustHandler returns the symbol a route's handler names, or "".
func resolveRustHandler(handler, routeFile string, byShort map[string][]rustFunction, crates []crateInfo) string {
	segs := strings.Split(handler, "::")
	fn := segs[len(segs)-1]
	var module []string
	for _, s := range segs[:len(segs)-1] {
		switch s {
		case "", "crate", "self", "super":
		default:
			module = append(module, s)
		}
	}
	candidates := byShort[fn]
	if len(candidates) == 0 {
		return ""
	}
	crate := nearestCrateDir(factpath.Dir(routeFile), crates)
	inCrate := func(c rustFunction) bool {
		return nearestCrateDir(factpath.Dir(c.file), crates) == crate
	}

	if len(module) == 0 {
		for _, c := range candidates {
			if c.file == routeFile {
				return c.name
			}
		}
		return onlyName(candidates, inCrate)
	}
	inModule := func(c rustFunction) bool { return fileIsModule(c.file, module) }
	if t := onlyName(candidates, func(c rustFunction) bool { return inModule(c) && inCrate(c) }); t != "" {
		return t
	}
	return onlyName(candidates, inModule)
}

// onlyName returns the symbol name the candidates passing keep share, or "" when
// none passes or they name different symbols.
func onlyName(candidates []rustFunction, keep func(rustFunction) bool) string {
	found := ""
	for _, c := range candidates {
		if !keep(c) {
			continue
		}
		if found != "" && found != c.name {
			return ""
		}
		found = c.name
	}
	return found
}

// fileIsModule reports whether file declares the module a path ends in: its own path,
// without the extension and a trailing mod, ends in the path's segments.
func fileIsModule(file string, module []string) bool {
	parts := strings.Split(strings.TrimSuffix(factpath.Slash(file), ".rs"), "/")
	if n := len(parts); n > 0 && parts[n-1] == "mod" {
		parts = parts[:n-1]
	}
	if len(parts) < len(module) {
		return false
	}
	tail := parts[len(parts)-len(module):]
	for i := range module {
		if tail[i] != module[i] {
			return false
		}
	}
	return true
}
