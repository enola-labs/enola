// Package helmextractor extracts architectural facts from Helm charts.
package helmextractor

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/enola-labs/enola/internal/extractors/detectnames"
	"github.com/enola-labs/enola/internal/extractors/k8syaml"
	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
)

// A chart's structure is stated literally even though its output is not. Chart.yaml
// names the chart, its type and its dependencies; a chart's charts/ directory holds
// subcharts Helm loads with it; `{{ define "x" }}` declares a named template and
// `{{ include "x" . }}` and `{{ template "x" . }}` call it by that literal name; and
// the `kind:` a template file emits is, in practice, a literal line. None of that
// needs a template rendered, and nothing here renders one: what a template expands
// to depends on values given at install time, the rule the Ansible extractor follows
// for Jinja.
//
// A chart directory is a module. The chart is a symbol `<dir>.Chart.<name>`; each
// named template is `<dir>.template.<name>`; each resource a template file emits is
// `<dir>.<Kind>.<name>`, named by its literal metadata.name or, templated as it
// almost always is, by its file. CRDs under crds/ are plain YAML and read as objects.
//
// Named templates share one namespace across a chart and everything it loads, so an
// include resolves in the chart's own templates first, then across the charts it
// depends on, transitively, and draws an edge only when exactly one of them defines
// the name.

// Extractor extracts Helm facts.
type Extractor struct{}

// New creates the extractor.
func New() *Extractor { return &Extractor{} }

// Name returns the extractor name.
func (e *Extractor) Name() string { return "helm" }

// Detect walks the tree and answers from its names.
func (e *Extractor) Detect(repoPath string) (bool, error) {
	return e.DetectFiles(repoPath, detectnames.Walk(repoPath))
}

// DetectFiles implements plugin.FileListDetector: a Chart.yaml outside a dot
// directory.
func (e *Extractor) DetectFiles(_ string, files []string) (bool, error) {
	for _, rel := range files {
		if detectnames.Base(rel) == "Chart.yaml" && !hidden(rel) {
			return true, nil
		}
	}
	return false, nil
}

func hidden(rel string) bool {
	for _, seg := range strings.Split(rel, "/") {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}

type chartFile struct {
	APIVersion   string `yaml:"apiVersion"`
	Name         string `yaml:"name"`
	Version      string `yaml:"version"`
	AppVersion   any    `yaml:"appVersion"`
	Type         string `yaml:"type"`
	Dependencies []dep  `yaml:"dependencies"`
}

type dep struct {
	Name       string `yaml:"name"`
	Version    string `yaml:"version"`
	Repository string `yaml:"repository"`
	Alias      string `yaml:"alias"`
	Condition  string `yaml:"condition"`
}

type chart struct {
	dir     string
	file    string
	meta    chartFile
	sym     string
	deps    []*chart // resolved: subcharts, file:// and in-repo by name
	byName  []string // dependency names resolved by name to an in-repo chart
	remote  []string // dependency names left to a registry
	defines map[string]*define
	objects []*object
	// fileRefs carry the calls and value reads no define or resource holds: those
	// in NOTES.txt, in a file with no literal kind, or outside every define of a .tpl.
	fileRefs map[string]*refs
	values   []valueKey
}

// refs are the named templates a carrier calls and the value paths it reads.
// named are dotted value paths written as strings ("server.masterKey" handed to a
// helper that looks the value up), which count only when the chart declares them.
type refs struct {
	calls []string
	reads [][]string
	named [][]string
}

func (r *refs) add(line string) {
	for _, cm := range callsIn(line) {
		r.calls = append(r.calls, cm[1])
	}
	r.reads = append(r.reads, readsIn(line)...)
	r.named = append(r.named, pathStringsIn(line)...)
}

func (r *refs) merge(o refs) {
	r.calls = append(r.calls, o.calls...)
	r.reads = append(r.reads, o.reads...)
	r.named = append(r.named, o.named...)
}

type define struct {
	refs
	name, file string
	line       int
	sym        string
}

type object struct {
	refs
	kind, name, file string
	line             int
	sym              string
	helmKind         string // resource | crd
	apiVersion       string
}

// Extract walks the repository itself: YAML is ignore-globbed by default.
func (e *Extractor) Extract(_ context.Context, repoPath string, _ []string) ([]facts.Fact, error) {
	files := k8syaml.Walk(repoPath, func(rel string) bool {
		return k8syaml.IsYAML(rel) || strings.HasSuffix(rel, ".tpl") || strings.HasSuffix(rel, ".txt")
	}, nil)
	sort.Strings(files)

	charts := map[string]*chart{}
	for _, rel := range files {
		if detectnames.Base(rel) != "Chart.yaml" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(repoPath, rel))
		if err != nil {
			continue
		}
		var meta chartFile
		if yaml.Unmarshal(data, &meta) != nil || meta.Name == "" || !k8syaml.Literal(meta.Name) {
			continue
		}
		dir := factpath.Dir(rel)
		if len(meta.Dependencies) == 0 {
			// A Helm 2 chart lists its dependencies in requirements.yaml.
			if req, err := os.ReadFile(filepath.Join(repoPath, filepath.FromSlash(prefix(dir)+"requirements.yaml"))); err == nil {
				var r chartFile
				if yaml.Unmarshal(req, &r) == nil {
					meta.Dependencies = r.Dependencies
				}
			}
		}
		c := &chart{dir: dir, file: rel, meta: meta, defines: map[string]*define{}, fileRefs: map[string]*refs{}}
		c.sym = qualify(dir, "Chart."+meta.Name)
		charts[dir] = c
	}
	if len(charts) == 0 {
		return nil, nil
	}
	dirs := make([]string, 0, len(charts))
	for d := range charts {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)

	for _, rel := range files {
		c := owner(rel, charts)
		if c == nil || rel == c.file {
			continue
		}
		inside := strings.TrimPrefix(rel, prefix(c.dir))
		data, err := os.ReadFile(filepath.Join(repoPath, rel))
		if err != nil {
			continue
		}
		switch {
		case inside == "values.yaml":
			c.values = readValues(data)
			// A values string can itself be a template, rendered with tpl: bitnami's
			// rabbitmq builds its configuration in values.yaml from other values.
			vr := &refs{}
			for _, act := range actionLines(string(data)) {
				vr.add(act)
			}
			if len(vr.calls) > 0 || len(vr.reads) > 0 || len(vr.named) > 0 {
				c.fileRefs[rel] = vr
			}
		case strings.HasPrefix(inside, "templates/"):
			readTemplate(c, rel, string(data))
		case strings.HasPrefix(inside, "crds/") && k8syaml.IsYAML(rel):
			for _, o := range k8syaml.Decode(data, rel) {
				c.objects = append(c.objects, &object{
					kind: o.Kind, name: o.Name, file: rel, line: o.Line,
					helmKind: "crd", apiVersion: o.APIVersion,
				})
			}
		}
	}
	resolveDependencies(charts, dirs)
	return emit(charts, dirs), nil
}

func qualify(dir, name string) string {
	if dir == "." {
		return name
	}
	return dir + "." + name
}

func prefix(dir string) string {
	if dir == "." {
		return ""
	}
	return dir + "/"
}

// owner is the chart a file belongs to: the nearest directory above it holding a
// Chart.yaml.
func owner(rel string, charts map[string]*chart) *chart {
	for dir := factpath.Dir(rel); ; dir = factpath.Dir(dir) {
		if c := charts[dir]; c != nil {
			return c
		}
		if dir == "." || dir == "/" || dir == "" {
			return nil
		}
	}
}

var (
	defineRe  = regexp.MustCompile(`\{\{-?\s*define\s+"([^"]+)"\s*-?\}\}`)
	actionRe  = regexp.MustCompile(`\{\{.*?\}\}`)
	callRe    = regexp.MustCompile(`\b(?:include|template)\s+"([A-Za-z0-9_][^"\s{}$]*)"`)
	endRe     = regexp.MustCompile(`\{\{-?\s*end\s*-?\}\}`)
	blockOpen = regexp.MustCompile(`\{\{-?\s*(?:if|range|with|define|block)\b`)
	kindLine  = regexp.MustCompile(`^kind:\s*["']?([A-Za-z][A-Za-z0-9]*)["']?\s*$`)
	apiLine   = regexp.MustCompile(`^apiVersion:\s*(?:["']?([^"'\s{]+)["']?|\{\{.*\}\})\s*$`)
	nameLine  = regexp.MustCompile(`^  name:\s*["']?([^"'\s{]+)["']?\s*$`)
	// The root context reaches .Values as `.Values`, `$.Values`, through a variable
	// holding it (`$root.Values`, `$global.Values`), or through a dict key it was
	// passed under (`.context.Values`, bitnami's idiom).
	// A keyword may be glued to it (`with.Values.x` parses).
	valuesRe   = regexp.MustCompile(`(?:^|[^\w$.]|\b(?:with|if|range|not|and|or))(?:\$\w*)?(?:\.\w+)*?\.Values((?:\.[A-Za-z_][A-Za-z0-9_]*)*)`)
	indexRe    = regexp.MustCompile(`\bindex\s+(?:\$\w*)?(?:\.\w+)*?\.Values((?:\.[A-Za-z_][A-Za-z0-9_]*)*)((?:\s+"[^"]+")*)`)
	quotedRe   = regexp.MustCompile(`"([^"]+)"`)
	dottedPath = regexp.MustCompile(`^[A-Za-z_][\w-]*(?:\.[A-Za-z_][\w-]*)+$`)
)

// actionLines returns, for each line of a template, the template-action text on it,
// each piece wrapped back in {{ }}. An action may span lines (a dict built over
// several, one key per line), and its continuation lines are action text too; a
// line outside every action contributes nothing, so text a template emits is never
// read as a call or a value.
func actionLines(text string) []string {
	lines := strings.Split(text, "\n")
	out := make([]string, len(lines))
	inside := false
	for i, ln := range lines {
		var b strings.Builder
		rest := ln
		for rest != "" {
			if !inside {
				open := strings.Index(rest, "{{")
				if open < 0 {
					break
				}
				rest = rest[open+2:]
				inside = true
				continue
			}
			end := strings.Index(rest, "}}")
			if end < 0 {
				b.WriteString("{{" + rest + "}}")
				rest = ""
				break
			}
			b.WriteString("{{" + rest[:end] + "}}")
			rest = rest[end+2:]
			inside = false
		}
		out[i] = b.String()
	}
	return out
}

// pathStringsIn finds quoted strings inside a line's template actions shaped like a
// dotted value path.
func pathStringsIn(ln string) [][]string {
	var out [][]string
	for _, action := range actionRe.FindAllString(ln, -1) {
		for _, q := range quotedRe.FindAllStringSubmatch(action, -1) {
			if dottedPath.MatchString(q[1]) {
				out = append(out, strings.Split(q[1], "."))
			}
		}
	}
	return out
}

// readsIn finds the value paths a line's template actions read: `.Values.a.b` in
// any of the forms valuesRe accepts, and `index .Values.a "b" "c"`, whose literal keys
// extend the path and whose variable keys leave it at the map they index. A bare
// `.Values` is a read of every value, and comes back as an empty path.
func readsIn(ln string) [][]string {
	var out [][]string
	for _, action := range actionRe.FindAllString(ln, -1) {
		for _, m := range indexRe.FindAllStringSubmatchIndex(action, -1) {
			path := splitDotted(action[m[2]:m[3]])
			for _, q := range quotedRe.FindAllStringSubmatch(action[m[4]:m[5]], -1) {
				path = append(path, q[1])
			}
			out = append(out, path)
		}
		for _, m := range valuesRe.FindAllStringSubmatchIndex(action, -1) {
			if inIndex(action, m[0]) {
				continue
			}
			out = append(out, splitDotted(action[m[2]:m[3]]))
		}
	}
	return out
}

func splitDotted(p string) []string {
	if p == "" {
		return []string{}
	}
	return strings.Split(strings.TrimPrefix(p, "."), ".")
}

// inIndex reports whether the .Values at pos is the subject of an index call, which
// readsIn has already read with its keys.
func inIndex(action string, pos int) bool {
	before := strings.TrimRight(action[:pos+1], " ")
	return strings.HasSuffix(before, "index") || strings.HasSuffix(strings.TrimRight(action[:pos], " "), "index")
}

// callsIn finds the named-template calls inside a line's template actions. A call
// outside {{ }} is text the template emits, such as a config file that happens to
// say `template "x"`, and is not a call.
func callsIn(ln string) [][]string {
	var out [][]string
	for _, action := range actionRe.FindAllString(ln, -1) {
		out = append(out, callRe.FindAllStringSubmatch(action, -1)...)
	}
	return out
}

// readTemplate reads one file under templates/: the named templates it defines, the
// resources its documents emit, and the calls and value reads of each. What falls
// outside every define and every resource belongs to the file.
func readTemplate(c *chart, rel, text string) {
	lines := strings.Split(text, "\n")
	acts := actionLines(text)
	file := &refs{}
	isYAML := k8syaml.IsYAML(rel)
	var cur *define
	depth := 0
	// A document may hold several literal kinds, each in its own branch of an if/else
	// (victoriametrics emits a DaemonSet or a Deployment from one file, mariadb one of
	// two Secrets). Each is a resource the chart can create, and all of them share the
	// document's calls and reads. obj is the one the lines being read belong to.
	var objs []*object
	var obj *object
	var doc refs
	inMetadata := false
	pendingAPI, apiLineNo := "", 0
	flush := func() {
		if len(objs) == 0 {
			file.merge(doc)
		}
		for _, o := range objs {
			o.refs = refs{
				calls: append([]string{}, doc.calls...),
				reads: append([][]string{}, doc.reads...),
				named: append([][]string{}, doc.named...),
			}
			c.objects = append(c.objects, o)
		}
		objs, obj, doc, inMetadata, pendingAPI, apiLineNo = nil, nil, refs{}, false, "", 0
	}
	for i, ln := range lines {
		lineNo := i + 1
		if cur == nil {
			if m := defineRe.FindStringSubmatch(ln); m != nil {
				at := strings.Index(ln, m[0])
				d := &define{name: m[1], file: rel, line: lineNo}
				if _, dup := c.defines[d.name]; !dup {
					c.defines[d.name] = d
				}
				cur, depth = d, 0
				rest := ln[at+len(m[0]):]
				d.add(acts[i])
				depth += len(blockOpen.FindAllString(rest, -1))
				if ends := len(endRe.FindAllString(rest, -1)); ends > depth {
					cur = nil
				} else {
					depth -= ends
				}
				continue
			}
		}
		if cur != nil {
			cur.add(acts[i])
			depth += len(blockOpen.FindAllString(ln, -1))
			depth -= len(endRe.FindAllString(ln, -1))
			if depth < 0 {
				cur = nil
			}
			continue
		}
		if !isYAML {
			file.add(acts[i])
			continue
		}
		if strings.HasPrefix(ln, "---") {
			flush()
			continue
		}
		doc.add(acts[i])
		if m := kindLine.FindStringSubmatch(ln); m != nil {
			// The object sits where it starts: the apiVersion line when one came first,
			// as it usually does, else the kind line.
			start := lineNo
			if apiLineNo > 0 {
				start = apiLineNo
			}
			obj = &object{kind: m[1], file: rel, line: start, helmKind: "resource", apiVersion: pendingAPI}
			objs = append(objs, obj)
			pendingAPI, apiLineNo, inMetadata = "", 0, false
			continue
		}
		if m := apiLine.FindStringSubmatch(ln); m != nil {
			pendingAPI, apiLineNo = m[1], lineNo
			continue
		}
		if obj == nil {
			continue
		}
		switch {
		case strings.HasPrefix(ln, "metadata:"):
			inMetadata = true
		case inMetadata && obj.name == "":
			if m := nameLine.FindStringSubmatch(ln); m != nil && k8syaml.Literal(m[1]) {
				obj.name = m[1]
			}
		}
		if ln != "" && ln[0] != ' ' && ln[0] != '#' && ln[0] != '{' && !strings.HasPrefix(ln, "metadata:") {
			inMetadata = false
		}
	}
	flush()
	if len(file.calls) > 0 || len(file.reads) > 0 || len(file.named) > 0 {
		c.fileRefs[rel] = file
	}
}
