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
}

type define struct {
	name, file string
	line       int
	sym        string
	calls      []string
}

type object struct {
	kind, name, file string
	line             int
	sym              string
	helmKind         string // resource | crd
	apiVersion       string
	calls            []string
}

// Extract walks the repository itself: YAML is ignore-globbed by default.
func (e *Extractor) Extract(_ context.Context, repoPath string, _ []string) ([]facts.Fact, error) {
	files := k8syaml.Walk(repoPath, func(rel string) bool {
		return k8syaml.IsYAML(rel) || strings.HasSuffix(rel, ".tpl") || detectnames.Base(rel) == "Chart.yaml"
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
		c := &chart{dir: dir, file: rel, meta: meta, defines: map[string]*define{}}
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
)

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
// calls inside each, and the resources its documents emit with the calls outside
// any define.
func readTemplate(c *chart, rel, text string) {
	lines := strings.Split(text, "\n")
	var cur *define
	depth := 0
	var obj *object
	var docCalls []string
	inMetadata := false
	pendingAPI, apiLineNo := "", 0
	flush := func() {
		if obj != nil {
			obj.calls = docCalls
			c.objects = append(c.objects, obj)
		}
		obj, docCalls, inMetadata, pendingAPI, apiLineNo = nil, nil, false, "", 0
	}
	for i, ln := range lines {
		lineNo := i + 1
		if cur == nil {
			if m := defineRe.FindStringSubmatch(ln); m != nil {
				name := m[1]
				d := &define{name: name, file: rel, line: lineNo}
				if _, dup := c.defines[name]; !dup {
					c.defines[name] = d
				}
				cur, depth = d, 0
				rest := ln[strings.Index(ln, m[0])+len(m[0]):]
				depth += len(blockOpen.FindAllString(rest, -1))
				ends := len(endRe.FindAllString(rest, -1))
				for _, cm := range callsIn(rest) {
					d.calls = append(d.calls, cm[1])
				}
				if ends > depth {
					cur = nil
				} else {
					depth -= ends
				}
				continue
			}
		}
		if cur != nil {
			for _, cm := range callsIn(ln) {
				cur.calls = append(cur.calls, cm[1])
			}
			depth += len(blockOpen.FindAllString(ln, -1))
			depth -= len(endRe.FindAllString(ln, -1))
			if depth < 0 {
				cur = nil
			}
			continue
		}
		if !k8syaml.IsYAML(rel) {
			continue
		}
		if strings.HasPrefix(ln, "---") {
			flush()
			continue
		}
		for _, cm := range callsIn(ln) {
			docCalls = append(docCalls, cm[1])
		}
		if m := kindLine.FindStringSubmatch(ln); m != nil && obj == nil {
			// The object sits where its document starts: the apiVersion line when
			// one came first, as it usually does, else the kind line.
			start := lineNo
			if apiLineNo > 0 {
				start = apiLineNo
			}
			obj = &object{kind: m[1], file: rel, line: start, helmKind: "resource", apiVersion: pendingAPI}
			continue
		}
		if obj == nil {
			if m := apiLine.FindStringSubmatch(ln); m != nil {
				pendingAPI, apiLineNo = m[1], lineNo
			}
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
		if m := apiLine.FindStringSubmatch(ln); m != nil && m[1] != "" && obj.apiVersion == "" {
			obj.apiVersion = m[1]
		}
		if ln != "" && ln[0] != ' ' && ln[0] != '#' && ln[0] != '{' && !strings.HasPrefix(ln, "metadata:") {
			inMetadata = false
		}
	}
	flush()
}
