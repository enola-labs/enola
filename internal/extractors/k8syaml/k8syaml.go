// Package k8syaml reads Kubernetes objects out of YAML files, for the kubernetes and
// helm extractors.
//
// It decides two things both extractors need the same answer to: which files a
// self-walking YAML extractor may read, and whether a YAML document is a Kubernetes
// object at all. Arbitrary YAML (CI workflows, OpenAPI specs, compose files, a tool's
// own config) must not read as a manifest, so a document counts only when it states
// an apiVersion Kubernetes serves, a kind, and a literal metadata.name.
package k8syaml

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/workspace"
)

// Object is one Kubernetes object as a file states it.
type Object struct {
	APIVersion string
	Kind       string
	Name       string
	// Namespace is metadata.namespace when the file states it, else empty.
	Namespace string
	File      string
	Line      int
	// Doc is the whole decoded document.
	Doc map[string]any
}

// skipDirs are the directories a self-walking extractor must not enter: the ones the
// engine's own walk prunes, which hold vendored or generated copies.
var skipDirs = map[string]bool{
	"node_modules": true, "vendor": true, "testdata": true, "dist": true,
	"build": true, "tmp": true, "log": true, "coverage": true,
}

// IsYAML reports whether a path is a YAML file by its extension.
func IsYAML(p string) bool {
	return strings.HasSuffix(p, ".yaml") || strings.HasSuffix(p, ".yml")
}

// Walk lists the files under repoPath that keep(rel) accepts, repo-relative and
// forward-slashed, skipping dot directories, the engine's pruned directories and
// linked worktrees. skipDir, when non-nil, prunes further directories by their
// absolute path; the kubernetes extractor uses it to leave a chart's tree to helm.
func Walk(repoPath string, keep func(rel string) bool, skipDir func(abs string) bool) []string {
	var files []string
	root := filepath.Clean(repoPath) //factpath:host
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path == root {
				return nil
			}
			name := d.Name()
			if strings.HasPrefix(name, ".") || skipDirs[name] || workspace.IsLinkedWorktree(path) {
				return filepath.SkipDir
			}
			if skipDir != nil && skipDir(path) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		rel = factpath.Slash(rel)
		if keep(rel) {
			files = append(files, rel)
		}
		return nil
	})
	return files
}

// legacyGroups are the API groups Kubernetes serves without a dot in their name.
var legacyGroups = map[string]bool{
	"apps": true, "batch": true, "autoscaling": true, "policy": true, "extensions": true,
}

// notObjects are API groups whose documents are configuration for a tool, not
// objects a cluster stores.
var notObjects = []string{
	"kustomize.config.k8s.io", "config.kubernetes.io", "kind.x-k8s.io",
	"kubeadm.k8s.io", "kubelet.config.k8s.io", "kubeproxy.config.k8s.io",
	"apiserver.config.k8s.io",
}

// IsAPIVersion reports whether apiVersion names a version Kubernetes serves: the core
// "v1", a legacy group ("apps/v1"), or a dotted group ("networking.k8s.io/v1",
// "argoproj.io/v1alpha1"). skaffold/v4beta6 and other undotted tool groups fail.
func IsAPIVersion(apiVersion string) bool {
	if apiVersion == "v1" {
		return true
	}
	group, version, ok := strings.Cut(apiVersion, "/")
	if !ok || group == "" || version == "" || strings.Contains(version, "/") {
		return false
	}
	if !strings.HasPrefix(version, "v") {
		return false
	}
	for _, g := range notObjects {
		if group == g {
			return false
		}
	}
	return legacyGroups[group] || strings.Contains(group, ".")
}

// Literal reports whether a scalar is a value the file states, not a template
// expression or a kustomize/envsubst placeholder.
func Literal(s string) bool {
	return s != "" && !strings.Contains(s, "{{") && !strings.Contains(s, "${") && !strings.Contains(s, "$(")
}

// MaybeObjects is the cheap byte prefilter: a file with no top-level apiVersion and
// kind lines cannot hold an object, so it is not decoded at all.
func MaybeObjects(data []byte) bool {
	return hasTopKey(data, "apiVersion:") && hasTopKey(data, "kind:")
}

func hasTopKey(data []byte, key string) bool {
	k := []byte(key)
	if bytes.HasPrefix(data, k) {
		return true
	}
	return bytes.Contains(data, append([]byte{'\n'}, k...))
}

// Decode reads every Kubernetes object in a multi-document YAML file. A document that
// fails to parse ends the file: what follows a broken document cannot be located
// reliably. A `kind: List` (or any *List) contributes its items.
func Decode(data []byte, file string) []Object {
	if !MaybeObjects(data) {
		return nil
	}
	var out []Object
	dec := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var node yaml.Node
		err := dec.Decode(&node)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			break
		}
		if node.Kind != yaml.DocumentNode || len(node.Content) == 0 {
			continue
		}
		top := node.Content[0]
		if top.Kind != yaml.MappingNode {
			continue
		}
		var doc map[string]any
		if top.Decode(&doc) != nil {
			continue
		}
		out = append(out, objectsOf(doc, file, top.Line, top)...)
	}
	return out
}

func objectsOf(doc map[string]any, file string, line int, node *yaml.Node) []Object {
	apiVersion, _ := doc["apiVersion"].(string)
	kind, _ := doc["kind"].(string)
	if !IsAPIVersion(apiVersion) || kind == "" {
		return nil
	}
	if strings.HasSuffix(kind, "List") {
		items, _ := doc["items"].([]any)
		itemNodes := listItemNodes(node)
		var out []Object
		for i, it := range items {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			l := line
			if i < len(itemNodes) {
				l = itemNodes[i].Line
			}
			out = append(out, objectsOf(m, file, l, nil)...)
		}
		return out
	}
	meta, _ := doc["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	if !Literal(name) {
		return nil
	}
	ns, _ := meta["namespace"].(string)
	if !Literal(ns) {
		ns = ""
	}
	return []Object{{APIVersion: apiVersion, Kind: kind, Name: name, Namespace: ns, File: file, Line: line, Doc: doc}}
}

func listItemNodes(node *yaml.Node) []*yaml.Node {
	if node == nil {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == "items" && node.Content[i+1].Kind == yaml.SequenceNode {
			return node.Content[i+1].Content
		}
	}
	return nil
}

// Map returns doc[path...] as a map, or nil.
func Map(doc map[string]any, path ...string) map[string]any {
	cur := doc
	for _, p := range path {
		next, ok := cur[p].(map[string]any)
		if !ok {
			return nil
		}
		cur = next
	}
	return cur
}

// List returns doc[path...] as a list, or nil.
func List(doc map[string]any, path ...string) []any {
	if len(path) == 0 {
		return nil
	}
	parent := Map(doc, path[:len(path)-1]...)
	if parent == nil {
		return nil
	}
	l, _ := parent[path[len(path)-1]].([]any)
	return l
}

// String returns doc[path...] as a literal string, or "".
func String(doc map[string]any, path ...string) string {
	if len(path) == 0 {
		return ""
	}
	parent := Map(doc, path[:len(path)-1]...)
	if parent == nil {
		return ""
	}
	s, _ := parent[path[len(path)-1]].(string)
	if !Literal(s) {
		return ""
	}
	return s
}

// Labels returns a string-valued label map, dropping templated values.
func Labels(m map[string]any) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		s, ok := v.(string)
		if !ok || !Literal(s) || !Literal(k) {
			continue
		}
		out[k] = s
	}
	return out
}
