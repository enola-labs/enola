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
	"poddisruptionbudget", "service.y", "svc.y", "pod.y",
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
	// generator is the kustomize field that generates the object, if one does. It is
	// not the engine's `generated` prop, which marks facts read from a file with a
	// code-generator banner.
	generator string
	// builds are the kustomizations whose closure holds this object's file, smallest
	// first.
	builds []*kustomization
	// app is the release an Argo CD Application states, nil for any other object.
	app *application
	// release is what the values files of an Application's remote chart state.
	release *release
}

// Extract walks the repository itself: YAML is ignore-globbed by default, the
// Ansible and OpenAPI precedent.
func (e *Extractor) Extract(_ context.Context, repoPath string, _ []string) ([]facts.Fact, error) {
	files := k8syaml.Walk(repoPath, func(rel string) bool {
		return k8syaml.IsYAML(rel) || factpath.Base(rel) == "Kustomization"
	}, func(abs string) bool {
		_, err := os.Stat(filepath.Join(abs, "Chart.yaml"))
		return err == nil
	})
	sort.Strings(files)

	ks := readKustomizations(repoPath, files)
	patchFiles := patchFileSet(ks)
	patchTargets := map[string][]patchTarget{}

	var objs []*obj
	seen := map[string]bool{}
	add := func(o *obj) {
		key := o.dir + "\x00" + o.Kind + "\x00" + o.Namespace + "\x00" + o.Name
		if !seen[key] {
			seen[key] = true
			objs = append(objs, o)
		}
	}
	for _, rel := range files {
		base := factpath.Base(rel)
		if isKustomization(strings.ToLower(base)) || base == "Kustomization" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(repoPath, rel))
		if err != nil {
			continue
		}
		decoded := k8syaml.Decode(data, rel)
		if patchFiles[rel] {
			// A patch is not an object: it changes the one it names.
			for _, o := range decoded {
				patchTargets[rel] = append(patchTargets[rel], patchTarget{o.Kind, o.Name})
			}
			continue
		}
		for _, o := range decoded {
			add(&obj{Object: o, dir: factpath.Dir(rel)})
		}
	}
	dirs := make([]string, 0, len(ks))
	for d := range ks {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	for _, d := range dirs {
		k := ks[d]
		for _, f := range k.patchFiles {
			k.patches = append(k.patches, patchTargets[f]...)
		}
		for _, g := range k.generated {
			add(&obj{
				Object: k8syaml.Object{
					APIVersion: "v1", Kind: g.kind, Name: g.name, Namespace: k.namespace,
					File: k.file, Line: g.line, Doc: map[string]any{},
				},
				dir:       k.dir,
				generator: g.generator,
			})
		}
	}
	closeBuilds(ks)
	holding := buildsHolding(ks)
	for _, o := range objs {
		o.builds = holding[o.File]
	}
	nameObjects(objs)
	for _, o := range objs {
		o.app = readApplication(o)
	}
	markLocal(repoPath, objs)
	readReleases(repoPath, files, objs, ks)
	return emit(objs, ks, dirs, buildContexts(repoPath, files)), nil
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

func sortedSet(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

func emit(objs []*obj, ks map[string]*kustomization, kdirs []string, contexts map[string]string) []facts.Fact {
	idx := newIndex(objs)
	var out []facts.Fact
	dirs := map[string]bool{}
	for _, o := range objs {
		dirs[o.dir] = true
	}
	for _, d := range kdirs {
		dirs[d] = true
	}
	for _, d := range sortedSet(dirs) {
		out = append(out, facts.Fact{
			Kind:  facts.KindModule,
			Name:  d,
			File:  d,
			Props: map[string]any{"language": "kubernetes"},
		})
	}
	for _, d := range kdirs {
		out = append(out, kustomizationFact(ks[d], ks, idx))
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
		if o.generator != "" {
			f.SetProp("generator", o.generator)
		}
		if imgs := images(o.Doc); len(imgs) > 0 {
			f.SetProp("images", imgs)
			if ctxs := contextsFor(imgs, contexts); len(ctxs) > 0 {
				// Read by the workload-source binder, which joins the directory to the
				// modules the other extractors found in it.
				f.SetProp(facts.PropBuildContexts, ctxs)
			}
		}
		if paths := ingressPaths(o); len(paths) > 0 {
			f.SetProp("ingress_paths", paths)
		}
		targets := map[string]bool{}
		refs := idx.references(o)
		if o.app != nil {
			for k, v := range o.app.props() {
				f.SetProp(k, v)
			}
			refs = append(refs, idx.applicationRefs(o, ks)...)
		}
		if o.release != nil {
			for k, v := range o.release.props() {
				f.SetProp(k, v)
			}
			refs = append(refs, idx.releaseRefs(o)...)
		}
		for _, t := range refs {
			if t != o.symbol {
				targets[t] = true
			}
		}
		for _, t := range sortedSet(targets) {
			f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelDependsOn, Target: t})
		}
		out = append(out, f)
	}
	return out
}

// kustomizationFact is a kustomization as a symbol: it depends on the objects in the
// files it lists, the kustomizations it includes, and the objects it patches.
func kustomizationFact(k *kustomization, ks map[string]*kustomization, idx *index) facts.Fact {
	f := facts.Fact{
		Kind: facts.KindSymbol,
		Name: k.sym,
		File: k.file,
		Line: 1,
		Props: map[string]any{
			"language":    "kubernetes",
			"symbol_kind": "type",
			"k8s_kind":    k.kind,
			"exported":    true,
		},
		Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: k.dir}},
	}
	if k.namespace != "" {
		f.SetProp("namespace", k.namespace)
	}
	if k.prefix != "" {
		f.SetProp("name_prefix", k.prefix)
	}
	if k.suffix != "" {
		f.SetProp("name_suffix", k.suffix)
	}
	if len(k.images) > 0 {
		f.SetProp("image_overrides", k.images)
	}
	if len(k.remote) > 0 {
		remote := append([]string{}, k.remote...)
		sort.Strings(remote)
		f.SetProp("remote_resources", remote)
	}
	targets := map[string]bool{}
	resources := map[string]bool{}
	for _, r := range k.resources {
		resources[r] = true
	}
	for _, o := range idx.all {
		if resources[o.File] || (o.generator != "" && o.File == k.file) {
			targets[o.symbol] = true
		}
	}
	for _, d := range k.subdirs {
		if sub := ks[d]; sub != nil && sub != k {
			targets[sub.sym] = true
		}
	}
	for _, c := range k.charts {
		targets[c] = true
	}
	if len(k.remoteCharts) > 0 {
		f.SetProp("remote_charts", k.remoteCharts)
	}
	for _, p := range k.patches {
		if t := idx.inBuild(k, p.kind, p.name); t != nil {
			targets[t.symbol] = true
		}
	}
	if len(k.patches) > 0 {
		f.SetProp("patches", len(k.patches))
	}
	for _, t := range sortedSet(targets) {
		f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelDependsOn, Target: t})
	}
	return f
}
