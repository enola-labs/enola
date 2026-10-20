package manifestextractor

import (
	"encoding/json"
	"strings"
)

// lockNames are the resolution files read beside a manifest. They are owned for
// caching but never parsed on their own: a lockfile with no manifest beside it
// describes a transitive closure this extractor has already decided not to
// carry.
var lockNames = map[string]bool{
	"package-lock.json": true,
	"yarn.lock":         true,
	"Gemfile.lock":      true,
	"Cargo.lock":        true,
	"pubspec.lock":      true,
}

// readGoMod reads a go.mod's require directives.
//
// Go is the one ecosystem where the manifest IS the lockfile: every require
// names an exact version, so nothing needs resolving. Lines marked `// indirect`
// are the transitive closure written into the same file, and they are skipped
// for the reason the package doc gives.
func readGoMod(rc *readCtx, relFile string) []pkgDep {
	var out []pkgDep
	inBlock := false
	for _, ln := range lines(rc.read(relFile)) {
		t := strings.TrimSpace(ln)
		switch {
		case t == "require (":
			inBlock = true
			continue
		case inBlock && t == ")":
			inBlock = false
			continue
		}
		var spec string
		switch {
		case inBlock:
			spec = t
		case strings.HasPrefix(t, "require "):
			spec = strings.TrimSpace(strings.TrimPrefix(t, "require "))
		default:
			continue
		}
		if spec == "" || strings.HasPrefix(spec, "//") {
			continue
		}
		if strings.Contains(spec, "// indirect") {
			continue
		}
		if i := strings.Index(spec, "//"); i >= 0 {
			spec = strings.TrimSpace(spec[:i])
		}
		name, version, ok := strings.Cut(spec, " ")
		if !ok {
			continue
		}
		version = strings.TrimSpace(version)
		out = append(out, pkgDep{
			Name: name, Ecosystem: EcosystemGo,
			Constraint: version, Resolved: version, Manifest: relFile,
		})
	}
	return out
}

// readPackageJSON reads dependencies and devDependencies, resolving each
// against package-lock.json when one sits beside it.
//
// A workspace's own packages appear in dependencies exactly as a registry
// package does, and nothing in the manifest distinguishes them. They are left
// in: a package a repository declares is a package it declares, and deciding
// otherwise would need the workspace globs — a second reading of the same file
// that could disagree with the first.
func readPackageJSON(rc *readCtx, relFile string) []pkgDep {
	var doc struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if err := json.Unmarshal([]byte(rc.read(relFile)), &doc); err != nil {
		return nil
	}
	resolved, unread := npmResolution(rc, relFile)
	var out []pkgDep
	for _, group := range []struct {
		deps map[string]string
		dev  bool
	}{{doc.Dependencies, false}, {doc.DevDependencies, true}} {
		for name, constraint := range group.deps {
			out = append(out, pkgDep{
				Name: name, Ecosystem: EcosystemNPM,
				Constraint: constraint, Resolved: resolved[name],
				Dev: group.dev, Manifest: relFile, LockUnread: unread,
			})
		}
	}
	return out
}

// npmResolution finds the nearest lockfile at or above an npm manifest and
// reads it. It returns the resolved versions and, when the nearest lockfile is
// one this extractor cannot read, its path — so a dependency it would have
// resolved is reported as unknown rather than as unpinned.
func npmResolution(rc *readCtx, relFile string) (map[string]string, string) {
	if resolved, path := rc.lock(relFile, "package-lock.json", npmLock); path != "" {
		return resolved, ""
	}
	if resolved, path := rc.lock(relFile, "yarn.lock", yarnLock); path != "" {
		return resolved, ""
	}
	// Named but not parsed: pnpm's lock is a nested YAML document whose shape
	// is a different problem, and bun's is binary. Naming them is what turns
	// "we did not resolve this" into a stated absence rather than a claim that
	// nothing resolved it.
	for _, name := range []string{"pnpm-lock.yaml", "bun.lockb"} {
		if path := rc.exists(relFile, name); path != "" {
			return nil, path
		}
	}
	return nil, ""
}

// yarnLock reads both yarn formats, which differ by less than they look.
//
// Classic writes an unindented `lodash@^4.17.0:` header and an indented
// `version "4.17.21"`. Berry writes `"lodash@npm:^4.17.0":` and
// `version: 4.17.21`. One scan handles both: an unindented line ending in `:`
// is a header naming one or more `name@range` specs, and the `version` line
// under it resolves every name in that header.
func yarnLock(text string) map[string]string {
	out := map[string]string{}
	var pending []string
	for _, ln := range lines(text) {
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		if !strings.HasPrefix(ln, " ") {
			pending = pending[:0]
			header, ok := strings.CutSuffix(strings.TrimSpace(ln), ":")
			if !ok {
				continue
			}
			for _, spec := range strings.Split(header, ",") {
				if name := yarnSpecName(spec); name != "" {
					pending = append(pending, name)
				}
			}
			continue
		}
		if len(pending) == 0 {
			continue
		}
		t := strings.TrimSpace(ln)
		rest, ok := strings.CutPrefix(t, "version")
		if !ok {
			continue
		}
		version := unquote(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rest), ":")))
		if version == "" {
			continue
		}
		for _, name := range pending {
			if out[name] == "" {
				out[name] = version
			}
		}
		pending = pending[:0]
	}
	return out
}

// yarnSpecName recovers the package name from one `name@range` spec.
//
// The separator is the last `@` BEFORE the range's protocol, not the last `@`
// in the spec. A scoped package's name opens with one, so the separator is
// never the first; and a Berry range carries a protocol (`npm:`, `patch:`,
// `workspace:`, `git+ssh:`) whose selector may carry more `@` of its own. An
// npm ALIAS is exactly that case: mastodon declares
// `"@typescript/native": "npm:typescript@7.0.2"`, whose lock descriptor is
// `@typescript/native@npm:typescript@7.0.2`. Reading the last `@` named the
// package `@typescript/native@npm:typescript`, which resolves against nothing,
// so a dependency the lockfile pins to exactly one version was reported as
// unpinned. A git spec (`foo@git+ssh://git@github.com/x`) failed the same way,
// on the `@` inside the URL.
//
// The colon is what locates the protocol. A classic-yarn spec has none
// (`lodash@^4.17.0`), and there the last `@` is still the separator.
func yarnSpecName(spec string) string {
	// A header quotes the WHOLE list when any spec in it needs quoting, so
	// splitting on the comma leaves the first spec with a lone opening quote
	// and the last with a lone closing one. unquote pairs them and so returns
	// both unchanged; trimming each end independently is what a per-spec read
	// needs. Mastodon's aliased typescript sits in exactly such a header.
	spec = strings.Trim(unquote(strings.TrimSpace(spec)), `"'`)
	if spec == "" {
		return ""
	}
	search := spec
	if colon := strings.IndexByte(spec, ':'); colon > 0 {
		search = spec[:colon]
	}
	at := strings.LastIndex(search, "@")
	if at <= 0 {
		return ""
	}
	return spec[:at]
}

// npmLock reads a v2/v3 package-lock's `packages` map. The v1 `dependencies`
// shape is read too, because a repository pinned to an old npm still has a real
// answer and reporting it as unpinned would be wrong about the tree on disk.
func npmLock(text string) map[string]string {
	var doc struct {
		Packages map[string]struct {
			Version string `json:"version"`
		} `json:"packages"`
		Dependencies map[string]struct {
			Version string `json:"version"`
		} `json:"dependencies"`
	}
	if err := json.Unmarshal([]byte(text), &doc); err != nil {
		return nil
	}
	out := map[string]string{}
	for path, entry := range doc.Packages {
		// Only the top-level install of a package answers for the manifest's
		// constraint; a nested node_modules/x/node_modules/y is a conflict
		// resolution for somebody else's dependency.
		name, ok := strings.CutPrefix(path, "node_modules/")
		if !ok || strings.Contains(name, "node_modules/") {
			continue
		}
		out[name] = entry.Version
	}
	for name, entry := range doc.Dependencies {
		if out[name] == "" {
			out[name] = entry.Version
		}
	}
	return out
}

// readGemfile reads `gem` declarations, resolving each against Gemfile.lock.
func readGemfile(rc *readCtx, relFile string) []pkgDep {
	resolved, _ := rc.lock(relFile, "Gemfile.lock", gemfileLock)
	var out []pkgDep
	for _, ln := range lines(rc.read(relFile)) {
		t := strings.TrimSpace(ln)
		if !strings.HasPrefix(t, "gem ") {
			continue
		}
		if i := strings.Index(t, "#"); i >= 0 {
			t = strings.TrimSpace(t[:i])
		}
		parts := splitRubyArgs(strings.TrimSpace(strings.TrimPrefix(t, "gem ")))
		if len(parts) == 0 {
			continue
		}
		name := unquote(parts[0])
		if name == "" {
			continue
		}
		constraint := ""
		for _, p := range parts[1:] {
			// A keyword argument (require:, group:, git:) is not a version.
			if strings.Contains(p, ":") && !strings.HasPrefix(strings.TrimSpace(p), "'") && !strings.HasPrefix(strings.TrimSpace(p), "\"") {
				continue
			}
			if v := unquote(p); v != "" {
				constraint = v
				break
			}
		}
		// A gem in a :development or :test group is a dev dependency. Read off
		// the same line only: tracking `group do ... end` blocks would need a
		// Ruby parser, and the extractor that has one does not read Gemfiles.
		dev := strings.Contains(t, ":development") || strings.Contains(t, ":test")
		out = append(out, pkgDep{
			Name: name, Ecosystem: EcosystemRubyGems,
			Constraint: constraint, Resolved: resolved[name],
			Dev: dev, Manifest: relFile,
		})
	}
	return out
}

// gemfileLock reads the resolved version of every spec in the lock's GEM
// section. Indentation is the grammar: a spec sits at four spaces, its own
// dependencies at six, and only the first is a resolved version.
func gemfileLock(text string) map[string]string {
	out := map[string]string{}
	inSpecs := false
	for _, ln := range lines(text) {
		if strings.TrimSpace(ln) == "specs:" {
			inSpecs = true
			continue
		}
		if ln != "" && !strings.HasPrefix(ln, " ") {
			inSpecs = false
			continue
		}
		if !inSpecs || !strings.HasPrefix(ln, "    ") || strings.HasPrefix(ln, "      ") {
			continue
		}
		name, rest, ok := strings.Cut(strings.TrimSpace(ln), " (")
		if !ok {
			continue
		}
		if version, ok := strings.CutSuffix(rest, ")"); ok && out[name] == "" {
			out[name] = version
		}
	}
	return out
}

// cargoDepKeywords are the section segments that make what follows a dependency.
var cargoDepKeywords = map[string]bool{
	"dependencies":       false, // value is `dev`
	"dev-dependencies":   true,
	"build-dependencies": true,
}

// readCargoToml reads Cargo's dependency sections in all three spellings:
//
//	[dependencies]              serde = "1.0"
//	[dependencies]              axum = { version = "0.7" }
//	[dependencies.windows-sys]  version = "0.52"
//
// The third is the one a naive section match misses, and it is not rare: it is
// how a dependency with many settings is written, and how a target-specific one
// usually is — tokio declares windows-sys under
// `[target.'cfg(windows)'.dependencies.windows-sys]` and nowhere else, so
// reading only the list form loses the dependency entirely rather than losing
// its version.
func readCargoToml(rc *readCtx, relFile string) []pkgDep {
	resolved, _ := rc.lock(relFile, "Cargo.lock", cargoLock)
	var out []pkgDep

	// listDev is set while inside a dependency LIST section; tableDep names the
	// package while inside a dependency TABLE section. At most one is active.
	inList, listDev := false, false
	tableDep, tableDev, tableVersion, tablePath := "", false, "", ""

	// flushTable emits the table section that just ended, if there was one.
	flushTable := func() {
		if tableDep == "" {
			return
		}
		out = append(out, pkgDep{
			Name: tableDep, Ecosystem: EcosystemCargo,
			Constraint: tableVersion, Resolved: resolved[tableDep],
			Dev: tableDev, Manifest: relFile, Path: tablePath,
		})
		tableDep, tableVersion, tablePath = "", "", ""
	}

	for _, ln := range lines(rc.read(relFile)) {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "[") {
			flushTable()
			inList, listDev = false, false
			name, dev, isDep := cargoDepSection(t)
			switch {
			case !isDep:
			case name == "":
				inList, listDev = true, dev
			default:
				tableDep, tableDev = name, dev
			}
			continue
		}
		if tableDep != "" {
			if v, ok := strings.CutPrefix(t, "version"); ok {
				if rest, ok := strings.CutPrefix(strings.TrimSpace(v), "="); ok {
					tableVersion = unquote(rest)
				}
			}
			if v, ok := strings.CutPrefix(t, "path"); ok {
				if rest, ok := strings.CutPrefix(strings.TrimSpace(v), "="); ok {
					tablePath = unquote(rest)
				}
			}
			continue
		}
		if !inList {
			continue
		}
		name, value, ok := strings.Cut(t, "=")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "" || strings.HasPrefix(name, "#") {
			continue
		}
		out = append(out, pkgDep{
			Name: name, Ecosystem: EcosystemCargo,
			Constraint: tomlVersion(value), Resolved: resolved[name],
			Dev: listDev, Manifest: relFile, Path: tomlInlineKey(value, "path"),
		})
	}
	flushTable()
	return out
}

// cargoDepSection classifies a TOML section header. It reports the package name
// when the header is a table for ONE dependency, empty when it is a list of
// them, and isDep false when it is neither.
//
// The keyword is found by segment rather than by prefix or suffix, because a
// target section puts arbitrary text before it — `target.'cfg(windows)'` — and
// the package name after it. Quoted segments are kept whole: a cfg expression
// may contain dots, and splitting through one would find a keyword that is not
// a section boundary.
func cargoDepSection(header string) (name string, dev, isDep bool) {
	segments := splitTOMLPath(strings.Trim(header, "[]"))
	for i := len(segments) - 1; i >= 0; i-- {
		isDev, ok := cargoDepKeywords[segments[i]]
		if !ok {
			continue
		}
		switch len(segments) - i {
		case 1:
			return "", isDev, true
		case 2:
			return segments[i+1], isDev, true
		default:
			// A keyword with more than one segment after it is not a shape
			// Cargo defines. Fail closed rather than guessing which is a name.
			return "", false, false
		}
	}
	return "", false, false
}

// splitTOMLPath splits a section path on dots outside quotes.
func splitTOMLPath(path string) []string {
	var out []string
	var buf strings.Builder
	quote := byte(0)
	for i := 0; i < len(path); i++ {
		c := path[i]
		switch {
		case quote != 0 && c == quote:
			quote = 0
		case quote == 0 && (c == '\'' || c == '"'):
			quote = c
		case quote == 0 && c == '.':
			out = append(out, strings.TrimSpace(buf.String()))
			buf.Reset()
			continue
		}
		buf.WriteByte(c)
	}
	out = append(out, strings.TrimSpace(buf.String()))
	return out
}

// tomlVersion pulls the version out of either Cargo spelling.
func tomlVersion(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "{") {
		if _, rest, ok := strings.Cut(value, "version"); ok {
			if _, v, ok := strings.Cut(rest, "\""); ok {
				v, _, _ = strings.Cut(v, "\"")
				return v
			}
		}
		return ""
	}
	return unquote(value)
}

// tomlInlineKey reads one quoted key of an inline table, `{ path = "../x" }`,
// and nothing from a bare string, which carries only a version.
func tomlInlineKey(value, key string) string {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "{") {
		return ""
	}
	for _, part := range strings.Split(strings.Trim(value, "{} "), ",") {
		k, v, ok := strings.Cut(part, "=")
		if ok && strings.TrimSpace(k) == key {
			return unquote(strings.TrimSpace(v))
		}
	}
	return ""
}

// cargoLock reads each [[package]] block's name and version.
func cargoLock(text string) map[string]string {
	out := map[string]string{}
	name := ""
	for _, ln := range lines(text) {
		t := strings.TrimSpace(ln)
		switch {
		case t == "[[package]]":
			name = ""
		case strings.HasPrefix(t, "name = "):
			name = unquote(strings.TrimPrefix(t, "name = "))
		case strings.HasPrefix(t, "version = ") && name != "":
			if out[name] == "" {
				out[name] = unquote(strings.TrimPrefix(t, "version = "))
			}
		}
	}
	return out
}

// readPubspec reads dependencies and dev_dependencies. Two-space indentation is
// the grammar, the same line scan dartextractor already reads a pubspec with —
// a pubspec is never deep enough to need a YAML parser for this question, and a
// package whose value is a map (a git or path dependency) has no version to pin.
func readPubspec(rc *readCtx, relFile string) []pkgDep {
	resolved, _ := rc.lock(relFile, "pubspec.lock", pubspecLock)
	var out []pkgDep
	section := ""
	for _, ln := range lines(rc.read(relFile)) {
		if ln == "" || strings.HasPrefix(strings.TrimSpace(ln), "#") {
			continue
		}
		if !strings.HasPrefix(ln, " ") {
			section = strings.TrimSuffix(strings.TrimSpace(ln), ":")
			continue
		}
		if section != "dependencies" && section != "dev_dependencies" {
			continue
		}
		// One level of indentation only: deeper lines belong to a map value.
		trimmed := strings.TrimLeft(ln, " ")
		if len(ln)-len(trimmed) > 2 {
			continue
		}
		name, constraint, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "" || name == "sdk" || name == "flutter" {
			continue
		}
		out = append(out, pkgDep{
			Name: name, Ecosystem: EcosystemPub,
			Constraint: unquote(strings.TrimSpace(constraint)), Resolved: resolved[name],
			Dev: section == "dev_dependencies", Manifest: relFile,
		})
	}
	return out
}

// pubspecLock reads each package's resolved version. A package's name sits at
// two spaces and its `version:` at four, so the nearest preceding name owns it.
func pubspecLock(text string) map[string]string {
	out := map[string]string{}
	name := ""
	for _, ln := range lines(text) {
		trimmed := strings.TrimLeft(ln, " ")
		if trimmed == "" {
			continue
		}
		switch indent := len(ln) - len(trimmed); {
		case indent == 2 && strings.HasSuffix(trimmed, ":"):
			name = strings.TrimSuffix(trimmed, ":")
		case indent == 4 && strings.HasPrefix(trimmed, "version:") && name != "":
			if out[name] == "" {
				out[name] = unquote(strings.TrimSpace(strings.TrimPrefix(trimmed, "version:")))
			}
		}
	}
	return out
}

// readRequirements reads a pip requirements file. There is no lockfile
// convention to resolve against — a requirements file pinned with == IS the
// lock, and one written with >= is simply not pinned.
func readRequirements(rc *readCtx, relFile string) []pkgDep {
	resolved, unread := pythonResolution(rc, relFile)
	var out []pkgDep
	for _, ln := range lines(rc.read(relFile)) {
		t := strings.TrimSpace(ln)
		if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "-") {
			continue
		}
		if i := strings.Index(t, "#"); i >= 0 {
			t = strings.TrimSpace(t[:i])
		}
		name, constraint := splitPythonRequirement(t)
		if name == "" {
			continue
		}
		out = append(out, pkgDep{
			Name: name, Ecosystem: EcosystemPyPI,
			Constraint: constraint, Resolved: pythonResolved(name, constraint, resolved),
			// A requirements file whose name says dev is a dev manifest. It is
			// the only signal the format carries, and it is a convention rather
			// than a rule, so it decides nothing but this flag.
			Dev:        strings.Contains(relFile, "dev") || strings.Contains(relFile, "test"),
			Manifest:   relFile,
			LockUnread: unread,
		})
	}
	return out
}

// readPyproject reads PEP 621 `[project] dependencies` and Poetry's
// `[tool.poetry.dependencies]`, which are the two spellings in the wild.
func readPyproject(rc *readCtx, relFile string) []pkgDep {
	resolved, unread := pythonResolution(rc, relFile)
	var out []pkgDep
	section := ""
	inArray := false
	for _, ln := range lines(rc.read(relFile)) {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "[") {
			section, inArray = strings.Trim(t, "[]"), false
			continue
		}
		switch section {
		case "project":
			if strings.HasPrefix(t, "dependencies") && strings.Contains(t, "[") {
				inArray = !strings.Contains(t, "]")
				out = append(out, pep621Entries(t, relFile, resolved, unread)...)
				continue
			}
			if inArray {
				if strings.Contains(t, "]") {
					inArray = false
				}
				out = append(out, pep621Entries(t, relFile, resolved, unread)...)
			}
		case "tool.poetry.dependencies", "tool.poetry.dev-dependencies", "tool.poetry.group.dev.dependencies":
			name, value, ok := strings.Cut(t, "=")
			if !ok {
				continue
			}
			name = strings.TrimSpace(name)
			if name == "" || name == "python" || strings.HasPrefix(name, "#") {
				continue
			}
			constraint := tomlVersion(value)
			out = append(out, pkgDep{
				Name: name, Ecosystem: EcosystemPyPI,
				Constraint: constraint, Resolved: pythonResolved(name, constraint, resolved),
				Dev:      section != "tool.poetry.dependencies",
				Manifest: relFile, LockUnread: unread,
			})
		}
	}
	return out
}

// pep621Entries reads the requirement strings on one line of a dependencies
// array.
//
// It extracts QUOTED SPANS rather than splitting on commas, because commas
// appear on both sides of the quotes and splitting is wrong in both directions.
// A single requirement may contain them —
// `"pydantic-settings>=2.2.1,!=2.12.*,<3"` is one dependency — and a line may
// carry a trailing `# comment` after its entry. Worse, an array is routinely
// interrupted by whole comment lines, and reading those as entries produced
// facts named `# GHSA-4xgf-cpjx-pc3j: 2.12.0–2.14.1 vulnerable` on cognee:
// thirty-three of them, every one a package that does not exist.
func pep621Entries(line, relFile string, resolved map[string]string, unread string) []pkgDep {
	var out []pkgDep
	for _, q := range quotedSpans(line) {
		name, constraint := splitPythonRequirement(q)
		if name == "" {
			continue
		}
		out = append(out, pkgDep{
			Name: name, Ecosystem: EcosystemPyPI,
			Constraint: constraint, Resolved: pythonResolved(name, constraint, resolved),
			Manifest: relFile, LockUnread: unread,
		})
	}
	return out
}

// quotedSpans returns the contents of every complete quoted string on a line.
// An unterminated quote yields nothing: half a requirement is not one.
func quotedSpans(line string) []string {
	var out []string
	for i := 0; i < len(line); i++ {
		q := line[i]
		if q != '\'' && q != '"' {
			continue
		}
		end := strings.IndexByte(line[i+1:], q)
		if end < 0 {
			return out
		}
		if span := strings.TrimSpace(line[i+1 : i+1+end]); span != "" {
			out = append(out, span)
		}
		i += end + 1
	}
	return out
}

// pythonResolved prefers the lockfile and falls back to an `==` pin written in
// the manifest itself, which is what a requirements file without a lock has.
func pythonResolved(name, constraint string, resolved map[string]string) string {
	if v := resolved[name]; v != "" {
		return v
	}
	// PyPI treats `_`, `.` and case as the same character, so a manifest may
	// spell a package differently from the lockfile that resolved it —
	// `typing_extensions` against `typing-extensions` — and a literal lookup
	// reports a resolved dependency as unpinned.
	if v := resolved[normalizePyPIName(name)]; v != "" {
		return v
	}
	if v, ok := strings.CutPrefix(constraint, "=="); ok && !strings.ContainsAny(v, ",*") {
		return strings.TrimSpace(v)
	}
	return ""
}

// pythonResolution finds the nearest Python lockfile at or above a manifest.
// poetry.lock and uv.lock are both TOML holding one `[[package]]` block per
// resolved package with a `name` and a `version`, which is the same shape
// Cargo.lock has — so they are read by the same scanner rather than by two more
// of their own.
func pythonResolution(rc *readCtx, relFile string) (map[string]string, string) {
	for _, name := range []string{"uv.lock", "poetry.lock"} {
		if resolved, path := rc.lock(relFile, name, pythonLock); path != "" {
			return resolved, ""
		}
	}
	// Pipenv's lock is JSON with a different shape, and pip-tools writes a
	// requirements file rather than a lock. Naming the one this extractor does
	// not read keeps "we did not resolve this" from reading as "nothing did".
	if path := rc.exists(relFile, "Pipfile.lock"); path != "" {
		return nil, path
	}
	return nil, ""
}

// pythonLock reads a uv or poetry lock — the Cargo `[[package]]` shape — and
// keys every entry by its normalized name as well as its written one, so a
// manifest spelling either way resolves.
func pythonLock(text string) map[string]string {
	out := cargoLock(text)
	for name, version := range out {
		if norm := normalizePyPIName(name); out[norm] == "" {
			out[norm] = version
		}
	}
	return out
}

// normalizePyPIName applies PEP 503: lowercase, with runs of `-`, `_` and `.`
// collapsed to a single `-`.
func normalizePyPIName(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		if r == '-' || r == '_' || r == '.' {
			dash = true
			continue
		}
		if dash && b.Len() > 0 {
			b.WriteByte('-')
		}
		dash = false
		b.WriteRune(r)
	}
	return b.String()
}

// splitPythonRequirement cuts a PEP 508 requirement into its name and its
// version specifier, dropping extras and environment markers — neither is a
// version, and neither changes whether one was pinned.
func splitPythonRequirement(spec string) (string, string) {
	if i := strings.Index(spec, ";"); i >= 0 {
		spec = spec[:i]
	}
	spec = strings.TrimSpace(spec)
	// A PEP 508 direct reference — `cognee @ file:///path` — names a package and
	// a place to get it. The place is not a version, and without this the whole
	// string became the package name.
	if name, _, ok := strings.Cut(spec, " @ "); ok {
		return strings.TrimSpace(name), ""
	}
	if i := strings.Index(spec, "["); i >= 0 {
		if j := strings.Index(spec, "]"); j > i {
			spec = spec[:i] + spec[j+1:]
		}
	}
	if i := strings.IndexAny(spec, "=<>!~"); i >= 0 {
		return strings.TrimSpace(spec[:i]), strings.TrimSpace(spec[i:])
	}
	return strings.TrimSpace(spec), ""
}

// splitRubyArgs splits a `gem` line's arguments on commas outside quotes, so a
// version containing a comma ('>= 1.0, < 2.0') stays one argument.
func splitRubyArgs(s string) []string {
	var out []string
	var buf strings.Builder
	quote := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0 && c == quote:
			quote = 0
		case quote == 0 && (c == '\'' || c == '"'):
			quote = c
		case quote == 0 && c == ',':
			out = append(out, strings.TrimSpace(buf.String()))
			buf.Reset()
			continue
		}
		buf.WriteByte(c)
	}
	if strings.TrimSpace(buf.String()) != "" {
		out = append(out, strings.TrimSpace(buf.String()))
	}
	return out
}

// unquote strips one layer of matching quotes.
func unquote(s string) string {
	s = strings.TrimSpace(s)
	for _, q := range []string{"'", "\""} {
		if len(s) >= 2 && strings.HasPrefix(s, q) && strings.HasSuffix(s, q) {
			return s[1 : len(s)-1]
		}
	}
	return s
}
