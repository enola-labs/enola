// Package k8sextractor extracts architectural facts from Kubernetes manifests.
package k8sextractor

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/extractors/detectnames"
	"github.com/enola-labs/enola/internal/extractors/k8syaml"
	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
)

// A manifest's structure is by name and by label, and both are literal: a Service
// selects the pods whose labels it names, an Ingress names the Service it sends to, a
// Deployment names the ConfigMaps, Secrets, claims and service account it mounts, a
// RoleBinding names its Role. Every edge below is one of those names, resolved
// against the objects the repository itself declares.
//
// A directory is a module, as it is for Terraform: it is the unit `kubectl apply -f`
// takes. A reference resolves to the one object of that kind and name in the same
// directory; failing that, to the one object of that kind and name anywhere in the
// repository; failing that, to nothing. Namespaces must agree when both sides state
// one. A missing edge beats a wrong one, so a name declared twice outside the
// referrer's directory draws nothing.
//
// Nothing is rendered or substituted: a templated or `${VAR}` name is not a name.
// Helm charts are left to the helm extractor, a kustomization is not an object, and
// an Ingress's paths are recorded on the Ingress, never as routes.

// Extractor extracts Kubernetes facts.
type Extractor struct{}

// New creates the extractor.
func New() *Extractor { return &Extractor{} }

// Name returns the extractor name.
func (e *Extractor) Name() string { return "kubernetes" }

// Detect walks the tree and answers from its names.
func (e *Extractor) Detect(repoPath string) (bool, error) {
	return e.DetectFiles(repoPath, detectnames.Walk(repoPath))
}

// deploySegments are directory names that hold manifests by convention.
var deploySegments = []string{
	"k8s", "kubernetes", "kube", "manifests", "kubernetes-manifests", "deploy",
	"deployment", "deployments", "overlays", "kustomize",
}

// kindWords are file-name fragments that name a manifest's kind.
var kindWords = []string{
	"deployment", "statefulset", "daemonset", "cronjob", "ingress", "configmap",
	"serviceaccount", "clusterrole", "rolebinding", "networkpolicy",
	"poddisruptionbudget", "service.y", "svc.y",
}

// DetectFiles implements plugin.FileListDetector. Detection is by name only, so that
// a repository's detected-extractor set does not flip on every YAML file it carries:
// a kustomization, or a YAML file under a directory conventionally holding
// manifests, or one whose name is a manifest kind. Files inside a Helm chart do not
// count; those are the helm extractor's. Extraction then reads content, and a
// detected repository with no object in it emits nothing.
func (e *Extractor) DetectFiles(_ string, files []string) (bool, error) {
	charts := chartDirsOf(files)
	for _, rel := range files {
		if !k8syaml.IsYAML(rel) || hiddenPath(rel) || underChart(rel, charts) {
			continue
		}
		base := strings.ToLower(detectnames.Base(rel))
		if isKustomization(base) {
			return true, nil
		}
		if detectnames.HasAnySegment(rel, deploySegments...) {
			return true, nil
		}
		for _, w := range kindWords {
			if strings.Contains(base, w) {
				return true, nil
			}
		}
	}
	return false, nil
}

func isKustomization(base string) bool {
	return base == "kustomization.yaml" || base == "kustomization.yml"
}

func hiddenPath(rel string) bool {
	for _, seg := range strings.Split(rel, "/") {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}

// chartDirsOf lists the directories holding a Chart.yaml.
func chartDirsOf(files []string) []string {
	var dirs []string
	for _, rel := range files {
		if detectnames.Base(rel) == "Chart.yaml" {
			dirs = append(dirs, factpath.Dir(rel))
		}
	}
	return dirs
}

func underChart(rel string, charts []string) bool {
	for _, d := range charts {
		if d == "." || strings.HasPrefix(rel, d+"/") {
			return true
		}
	}
	return false
}

// obj is an object with its fact name.
type obj struct {
	k8syaml.Object
	dir    string
	symbol string
}

// Extract walks the repository itself: YAML is ignore-globbed by default, the
// Ansible and OpenAPI precedent.
func (e *Extractor) Extract(_ context.Context, repoPath string, _ []string) ([]facts.Fact, error) {
	files := k8syaml.Walk(repoPath, k8syaml.IsYAML, func(abs string) bool {
		_, err := os.Stat(filepath.Join(abs, "Chart.yaml"))
		return err == nil
	})
	sort.Strings(files)

	var objs []*obj
	seen := map[string]bool{}
	for _, rel := range files {
		if isKustomization(strings.ToLower(detectnames.Base(rel))) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(repoPath, rel))
		if err != nil {
			continue
		}
		for _, o := range k8syaml.Decode(data, rel) {
			dir := factpath.Dir(rel)
			key := dir + "\x00" + o.Kind + "\x00" + o.Namespace + "\x00" + o.Name
			if seen[key] {
				continue
			}
			seen[key] = true
			objs = append(objs, &obj{Object: o, dir: dir})
		}
	}
	nameObjects(objs)
	return emit(objs), nil
}

// nameObjects gives each object its fact name, <dir>.<Kind>.<name>. Two objects of
// one kind and name in one directory differ only by namespace, so the namespace
// joins the name of every one of them.
func nameObjects(objs []*obj) {
	count := map[string]int{}
	base := func(o *obj) string {
		if o.dir == "." {
			return o.Kind + "." + o.Name
		}
		return o.dir + "." + o.Kind + "." + o.Name
	}
	for _, o := range objs {
		count[base(o)]++
	}
	for _, o := range objs {
		o.symbol = base(o)
		if count[o.symbol] > 1 && o.Namespace != "" {
			o.symbol += "@" + o.Namespace
		}
	}
}

func emit(objs []*obj) []facts.Fact {
	idx := newIndex(objs)
	var out []facts.Fact
	dirs := map[string]bool{}
	for _, o := range objs {
		dirs[o.dir] = true
	}
	dirList := make([]string, 0, len(dirs))
	for d := range dirs {
		dirList = append(dirList, d)
	}
	sort.Strings(dirList)
	for _, d := range dirList {
		out = append(out, facts.Fact{
			Kind:  facts.KindModule,
			Name:  d,
			File:  d,
			Props: map[string]any{"language": "kubernetes"},
		})
	}
	for _, o := range objs {
		f := facts.Fact{
			Kind: facts.KindSymbol,
			Name: o.symbol,
			File: o.File,
			Line: o.Line,
			Props: map[string]any{
				"language":    "kubernetes",
				"symbol_kind": "type",
				"k8s_kind":    o.Kind,
				"api_version": o.APIVersion,
				"exported":    true,
			},
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: o.dir}},
		}
		if o.Namespace != "" {
			f.SetProp("namespace", o.Namespace)
		}
		if imgs := images(o.Doc); len(imgs) > 0 {
			f.SetProp("images", imgs)
		}
		if paths := ingressPaths(o); len(paths) > 0 {
			f.SetProp("ingress_paths", paths)
		}
		targets := map[string]bool{}
		for _, t := range idx.references(o) {
			if t != o.symbol {
				targets[t] = true
			}
		}
		sorted := make([]string, 0, len(targets))
		for t := range targets {
			sorted = append(sorted, t)
		}
		sort.Strings(sorted)
		for _, t := range sorted {
			f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelDependsOn, Target: t})
		}
		out = append(out, f)
	}
	return out
}
