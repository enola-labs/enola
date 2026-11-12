package helmextractor

import (
	"strings"

	"gopkg.in/yaml.v3"
)

// valueKey is one key of a chart's values.yaml, as a path from the root.
type valueKey struct {
	path []string
	line int
}

// valueDepth is how deep into values.yaml keys become symbols. Two levels name
// what a chart exposes (`image.tag`, `service.port`, `persistence.enabled`) without
// turning every nested default of a large chart into a node: bitnami/charts holds
// tens of thousands of keys at any depth.
const valueDepth = 2

// readValues lists a values.yaml's keys down to valueDepth, each on its line.
func readValues(data []byte) []valueKey {
	var root yaml.Node
	if yaml.Unmarshal(data, &root) != nil || len(root.Content) == 0 {
		return nil
	}
	var out []valueKey
	var walk func(n *yaml.Node, path []string)
	walk = func(n *yaml.Node, path []string) {
		if n.Kind != yaml.MappingNode || len(path) >= valueDepth {
			return
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			if k.Kind != yaml.ScalarNode || k.Value == "" {
				continue
			}
			p := append(append([]string{}, path...), k.Value)
			out = append(out, valueKey{path: p, line: k.Line})
			walk(n.Content[i+1], p)
		}
	}
	walk(root.Content[0], nil)
	return out
}

// valueIndex is a chart's value symbols, keyed by their path segments. A key may
// itself hold dots (`prometheus.io/scrape` under annotations), so a path is never
// recovered by splitting its dotted form.
type valueIndex struct {
	keys     []valueSym
	byPath   map[string]*valueSym
	children map[string][]string
}

type valueSym struct {
	dotted string
	sym    string
	line   int
}

func pathKey(p []string) string { return strings.Join(p, "\x00") }

// newValueIndex builds the value symbols of a chart. `global` and every key naming a
// chart it loads (a dependency's name or alias, a subchart) are left out: they are
// values passed to other charts, read by those charts' templates, not this one's.
func newValueIndex(c *chart) *valueIndex {
	passthrough := map[string]bool{"global": true}
	for _, d := range c.meta.Dependencies {
		passthrough[d.Name] = true
		if d.Alias != "" {
			passthrough[d.Alias] = true
		}
	}
	for _, d := range c.deps {
		passthrough[d.meta.Name] = true
	}
	ix := &valueIndex{byPath: map[string]*valueSym{}, children: map[string][]string{}}
	for _, k := range c.values {
		if passthrough[k.path[0]] {
			continue
		}
		dotted := strings.Join(k.path, ".")
		ix.keys = append(ix.keys, valueSym{dotted: dotted, sym: qualify(c.dir, "values."+dotted), line: k.line})
		if len(k.path) > 1 {
			parent := pathKey(k.path[:len(k.path)-1])
			ix.children[parent] = append(ix.children[parent], ix.keys[len(ix.keys)-1].sym)
		}
	}
	j := 0
	for _, k := range c.values {
		if passthrough[k.path[0]] {
			continue
		}
		ix.byPath[pathKey(k.path)] = &ix.keys[j]
		j++
	}
	return ix
}

// resolve adds the value symbols a read of path names: the deepest declared key on
// the path and every declared key above it, and, when the read stops at a key, that
// key's declared children too, since passing `.Values.image` to a template reads
// all of image.
func (ix *valueIndex) resolve(path []string, into map[string]bool) {
	if len(path) == 0 {
		for _, k := range ix.keys {
			into[k.sym] = true
		}
		return
	}
	depth := 0
	for i := 1; i <= len(path) && i <= valueDepth; i++ {
		v := ix.byPath[pathKey(path[:i])]
		if v == nil {
			break
		}
		into[v.sym] = true
		depth = i
	}
	if depth > 0 && depth == len(path) {
		for _, child := range ix.children[pathKey(path[:depth])] {
			into[child] = true
		}
	}
}

// resolveDeclared resolves a value path written as a string, and only when the
// chart declares it to its full depth: a string that merely starts like a value
// ("common.names.fullname" in a chart with a `common` key) names nothing.
func (ix *valueIndex) resolveDeclared(path []string, into map[string]bool) {
	n := len(path)
	if n > valueDepth {
		n = valueDepth
	}
	if ix.byPath[pathKey(path[:n])] == nil {
		return
	}
	ix.resolve(path, into)
}
