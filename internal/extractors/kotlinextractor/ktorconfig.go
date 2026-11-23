package kotlinextractor

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/enola-labs/enola/internal/extractors/jvmsrc"
	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
)

// Ktor application modules named in configuration.
//
// A Ktor server started through EngineMain loads its modules by reflection from
// the `ktor.application.modules` list of application.conf (HOCON) or
// application.yaml, each entry the JVM name of a function:
//
//	ktor { application { modules = [ com.example.ApplicationKt.module ] } }
//
// Nothing in code names those functions, so the configuration is the reference.
// An entry binds only where the package, the class (a file's facade or a
// declared class or object) and the function all exist; anything else is a
// library's module and binds nothing.

// ktorConfigNames are the files EngineMain reads by default.
var ktorConfigNames = []string{"application.conf", "application.yaml", "application.yml"}

// ktorConfigFiles returns the default Ktor config files beside the production
// Kotlin source sets: `<module>/src/<set>/resources/application.*` for each
// `<module>/src/<set>/kotlin/` a Kotlin file sits in. They are read from disk
// rather than taken from the walked list, because a configuration that ignores
// YAML as data must not hide the wiring.
func ktorConfigFiles(repoPath string, kotlinFiles []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, rel := range kotlinFiles {
		if jvmsrc.IsTestSource(rel) || facts.IsTestPath(rel) {
			continue
		}
		res := resourcesDirFor(rel)
		if res == "" || seen[res] {
			continue
		}
		seen[res] = true
		for _, name := range ktorConfigNames {
			p := path.Join(res, name)
			if fi, err := os.Stat(filepath.Join(repoPath, filepath.FromSlash(p))); err == nil && !fi.IsDir() {
				out = append(out, p)
			}
		}
	}
	sort.Strings(out)
	return out
}

// resourcesDirFor returns `<module>/src/<set>/resources` for a file under
// `<module>/src/<set>/kotlin/`, or "".
func resourcesDirFor(rel string) string {
	segs := strings.Split(rel, "/")
	for i := 0; i+2 < len(segs); i++ {
		if segs[i] == "src" && segs[i+2] == "kotlin" {
			return strings.Join(append(append([]string{}, segs[:i+2]...), "resources"), "/")
		}
	}
	return ""
}

// isKtorConfigFile reports whether a path is a default Ktor config file.
func isKtorConfigFile(rel string) bool {
	base := path.Base(rel)
	for _, n := range ktorConfigNames {
		if base == n {
			return strings.HasSuffix(path.Dir(rel), "/resources") || path.Dir(rel) == "resources"
		}
	}
	return false
}

// extractKtorModuleRefs emits one file_ref fact per config file whose
// ktor.application.modules list names functions this repository declares.
func extractKtorModuleRefs(repoPath string, kotlinFiles []string, packageDirs map[string][]string, all []facts.Fact) []facts.Fact {
	configs := ktorConfigFiles(repoPath, kotlinFiles)
	if len(configs) == 0 {
		return nil
	}
	funcs := map[string][]string{} // function or method name -> the files declaring it
	for i := range all {
		f := &all[i]
		if f.Kind != facts.KindSymbol {
			continue
		}
		switch f.PropString("symbol_kind") {
		case facts.SymbolFunc, facts.SymbolMethod:
			funcs[f.Name] = append(funcs[f.Name], f.File)
		}
	}
	facades := map[string]string{} // file -> its JVM facade class
	facade := func(rel string) string {
		if c, ok := facades[rel]; ok {
			return c
		}
		c := jvmFacadeName(repoPath, rel)
		facades[rel] = c
		return c
	}

	var out []facts.Fact
	for _, cfg := range configs {
		src, err := os.ReadFile(filepath.Join(repoPath, filepath.FromSlash(cfg)))
		if err != nil {
			continue
		}
		var entries []string
		if strings.HasSuffix(cfg, ".conf") {
			entries = hoconKtorModules(src)
		} else {
			entries = yamlKtorModules(src)
		}
		var rels []facts.Relation
		seen := map[string]bool{}
		for _, e := range entries {
			target := resolveKtorModule(e, packageDirs, funcs, facade)
			if target == "" || seen[target] {
				continue
			}
			seen[target] = true
			rels = append(rels, facts.Relation{Kind: facts.RelCalls, Target: target})
		}
		if len(rels) == 0 {
			continue
		}
		out = append(out, facts.Fact{
			Kind:      facts.KindFileRef,
			Name:      cfg,
			File:      cfg,
			Line:      1,
			Props:     map[string]any{"language": "kotlin", "framework": "ktor"},
			Relations: rels,
		})
	}
	return out
}

// resolveKtorModule binds `pkg.Class.fn` to the function it names: a top-level
// function of the file whose facade is Class, or a member of a declared Class.
func resolveKtorModule(entry string, packageDirs map[string][]string, funcs map[string][]string, facade func(string) string) string {
	segs := strings.Split(entry, ".")
	if len(segs) < 3 {
		return ""
	}
	fn, class := segs[len(segs)-1], segs[len(segs)-2]
	pkg := strings.Join(segs[:len(segs)-2], ".")
	for _, dir := range packageDirs[pkg] {
		for _, file := range funcs[dir+"."+class+"."+fn] {
			if isKotlinFile(file) && factpath.Dir(file) == dir {
				return dir + "." + class + "." + fn
			}
		}
		for _, file := range funcs[dir+"."+fn] {
			if factpath.Dir(file) == dir && isKotlinFile(file) && facade(file) == class {
				return dir + "." + fn
			}
		}
	}
	return ""
}

// jvmFileNameRe reads a file's `@file:JvmName("Name")`.
var jvmFileNameRe = regexp.MustCompile(`^\s*@file\s*:\s*JvmName\s*\(\s*"([^"]+)"\s*\)`)

// jvmFacadeName is the class the JVM compiles a Kotlin file's top-level
// declarations into: its @file:JvmName, else kotlinFacadeName.
func jvmFacadeName(repoPath, rel string) string {
	if src, err := os.ReadFile(filepath.Join(repoPath, filepath.FromSlash(rel))); err == nil {
		for n, line := range strings.SplitN(string(src), "\n", 41) {
			if n == 40 {
				break
			}
			if m := jvmFileNameRe.FindStringSubmatch(line); m != nil {
				return m[1]
			}
			if packageRe.MatchString(line) {
				break // file annotations precede the package
			}
		}
	}
	return kotlinFacadeName(rel)
}

// yamlKtorModules reads ktor.application.modules from application.yaml.
func yamlKtorModules(src []byte) []string {
	var doc map[string]any
	if yaml.Unmarshal(src, &doc) != nil {
		return nil
	}
	cur := any(doc)
	for _, k := range []string{"ktor", "application", "modules"} {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[k]
	}
	list, _ := cur.([]any)
	var out []string
	for _, v := range list {
		if s, ok := v.(string); ok {
			out = append(out, strings.TrimSpace(s))
		}
	}
	return out
}

// hoconKtorModules reads ktor.application.modules from application.conf: nested
// objects, dotted keys, `=` or `:`, `#` and `//` comments, and quoted or bare
// entries in a list that may span lines. Includes and substitutions are not
// followed.
func hoconKtorModules(src []byte) []string {
	text := stripHoconComments(string(src))
	var out []string
	var stack []string // enclosing object paths, joined
	i := 0
	for i < len(text) {
		c := text[i]
		switch {
		case c == '}':
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			i++
		case isHoconKeyStart(c):
			j := i
			for j < len(text) && isHoconKeyChar(text[j]) {
				j++
			}
			key := strings.Trim(text[i:j], `"`)
			k := skipHoconSpace(text, j)
			full := key
			if len(stack) > 0 && stack[len(stack)-1] != "" {
				full = stack[len(stack)-1] + "." + key
			}
			if k < len(text) && (text[k] == '=' || text[k] == ':') {
				k = skipHoconSpace(text, k+1)
			}
			switch {
			case k < len(text) && text[k] == '{':
				stack = append(stack, full)
				i = k + 1
			case k < len(text) && text[k] == '[':
				end := strings.IndexByte(text[k:], ']')
				if end < 0 {
					return out
				}
				if full == "ktor.application.modules" {
					for _, e := range strings.FieldsFunc(text[k+1:k+end], func(r rune) bool {
						return r == ',' || r == '\n' || r == ' ' || r == '\t' || r == '\r'
					}) {
						if e = strings.Trim(e, `"`); e != "" {
							out = append(out, e)
						}
					}
				}
				i = k + end + 1
			default:
				i = skipHoconScalar(text, k)
			}
		default:
			i++
		}
	}
	return out
}

// skipHoconScalar skips a scalar value to the end of its line, over quoted
// strings and `${...}` substitutions, stopping before a closing brace.
func skipHoconScalar(s string, i int) int {
	for i < len(s) && s[i] != '\n' && s[i] != '}' && s[i] != ',' {
		switch {
		case s[i] == '"':
			if end := strings.IndexByte(s[i+1:], '"'); end >= 0 {
				i += end + 2
				continue
			}
		case s[i] == '$' && i+1 < len(s) && s[i+1] == '{':
			if end := strings.IndexByte(s[i:], '}'); end >= 0 {
				i += end + 1
				continue
			}
		}
		i++
	}
	return i
}

func isHoconKeyStart(c byte) bool {
	return c == '"' || c == '_' || c == '-' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func isHoconKeyChar(c byte) bool { return isHoconKeyStart(c) || c == '.' }

func skipHoconSpace(s string, i int) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\r' || s[i] == '\n') {
		i++
	}
	return i
}

// stripHoconComments drops `#` and `//` comments outside quoted strings.
func stripHoconComments(s string) string {
	var b strings.Builder
	for _, line := range strings.Split(s, "\n") {
		inQuote := false
		cut := len(line)
		for i := 0; i < len(line); i++ {
			switch {
			case line[i] == '"':
				inQuote = !inQuote
			case inQuote:
			case line[i] == '#':
				cut = i
			case line[i] == '/' && i+1 < len(line) && line[i+1] == '/':
				cut = i
			}
			if cut != len(line) {
				break
			}
		}
		b.WriteString(line[:cut])
		b.WriteByte('\n')
	}
	return b.String()
}
