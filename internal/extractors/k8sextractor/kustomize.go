package k8sextractor

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/enola-labs/enola/internal/extractors/k8syaml"
	"github.com/enola-labs/enola/internal/factpath"
)

// A kustomization is a build: it names the files and directories whose objects it
// emits (resources, and the older bases), the components it mixes in, the patches it
// applies to those objects, and the ConfigMaps and Secrets it generates. Each of
// those is a literal path or name, so a kustomization becomes a symbol of its own
// that depends on what it builds from, a patch becomes an edge to the object it
// patches rather than an object of its own, and a generated ConfigMap is an object a
// pod can name like any other.
//
// A build is also the scope a name means something in. An overlay and its base are
// applied together, so a reference resolves in the referrer's directory first, then
// in the smallest build holding the referrer, then in larger ones, and only then in
// the repository. Nothing is built: name prefixes and suffixes, a namespace override,
// image overrides and replacements are recorded on the kustomization and applied to
// no object, because one base serves every overlay and each would rename it
// differently.

type kustomization struct {
	dir  string
	file string
	kind string // Kustomization | Component
	sym  string

	resources  []string // local files, repo-relative
	subdirs    []string // local directories holding a kustomization, repo-relative
	remote     []string
	patchFiles []string // repo-relative
	patches    []patchTarget
	generated  []generatedObj
	namespace  string
	prefix     string
	suffix     string
	images     []string

	files map[string]bool // closure: every object file this build emits
	size  int
}

type patchTarget struct{ kind, name string }

type generatedObj struct {
	kind, name string
	line       int
}

type kustFile struct {
	APIVersion            string           `yaml:"apiVersion"`
	Kind                  string           `yaml:"kind"`
	Resources             []string         `yaml:"resources"`
	Bases                 []string         `yaml:"bases"`
	Components            []string         `yaml:"components"`
	Patches               []any            `yaml:"patches"`
	PatchesStrategicMerge []string         `yaml:"patchesStrategicMerge"`
	PatchesJSON6902       []map[string]any `yaml:"patchesJson6902"`
	Namespace             string           `yaml:"namespace"`
	NamePrefix            string           `yaml:"namePrefix"`
	NameSuffix            string           `yaml:"nameSuffix"`
	Images                []map[string]any `yaml:"images"`
	ConfigMapGenerator    []map[string]any `yaml:"configMapGenerator"`
	SecretGenerator       []map[string]any `yaml:"secretGenerator"`
}

func isRemote(p string) bool {
	return strings.Contains(p, "://") || strings.HasPrefix(p, "github.com/") ||
		strings.HasPrefix(p, "git@") || strings.Contains(p, "?ref=")
}

func kustomizationIn(repoPath, dir string) string {
	for _, n := range []string{"kustomization.yaml", "kustomization.yml", "Kustomization"} {
		if _, err := os.Stat(filepath.Join(repoPath, filepath.FromSlash(dir), n)); err == nil {
			return factpath.Join(dir, n)
		}
	}
	return ""
}

// readKustomizations parses every kustomization the walk found.
func readKustomizations(repoPath string, files []string) map[string]*kustomization {
	out := map[string]*kustomization{}
	for _, rel := range files {
		base := factpath.Base(rel)
		if !isKustomization(strings.ToLower(base)) && base != "Kustomization" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(repoPath, rel))
		if err != nil {
			continue
		}
		var kf kustFile
		if yaml.Unmarshal(data, &kf) != nil {
			continue
		}
		dir := factpath.Dir(rel)
		if _, dup := out[dir]; dup {
			continue
		}
		k := &kustomization{dir: dir, file: rel, kind: "Kustomization"}
		if kf.Kind == "Component" {
			k.kind = "Component"
		}
		k.sym = k.kind
		if dir != "." {
			k.sym = dir + "." + k.kind
		}
		local := func(p string) string { return factpath.Clean(factpath.Join(dir, p)) }
		entries := append(append(append([]string{}, kf.Resources...), kf.Bases...), kf.Components...)
		for _, r := range entries {
			if !k8syaml.Literal(r) {
				continue
			}
			if isRemote(r) {
				k.remote = append(k.remote, r)
				continue
			}
			p := local(r)
			if strings.HasPrefix(p, "../") || p == ".." {
				continue // outside the repository
			}
			if k8syaml.IsYAML(p) {
				k.resources = append(k.resources, p)
			} else if kustomizationIn(repoPath, p) != "" {
				k.subdirs = append(k.subdirs, p)
			}
		}
		for _, p := range kf.Patches {
			switch v := p.(type) {
			case string:
				k.patchFiles = append(k.patchFiles, local(v))
			case map[string]any:
				target := k8syaml.Map(v, "target")
				if s, _ := v["path"].(string); k8syaml.Literal(s) {
					k.patchFiles = append(k.patchFiles, local(s))
					if target == nil {
						continue
					}
				}
				if target != nil {
					k.patches = append(k.patches, patchTarget{str(target, "kind"), str(target, "name")})
				} else if s, _ := v["patch"].(string); s != "" {
					k.patches = append(k.patches, inlinePatchTargets(s)...)
				}
			}
		}
		for _, p := range kf.PatchesStrategicMerge {
			if strings.Contains(p, "\n") {
				k.patches = append(k.patches, inlinePatchTargets(p)...)
			} else if k8syaml.Literal(p) {
				k.patchFiles = append(k.patchFiles, local(p))
			}
		}
		for _, p := range kf.PatchesJSON6902 {
			if s, _ := p["path"].(string); k8syaml.Literal(s) {
				k.patchFiles = append(k.patchFiles, local(s))
			}
			if t := k8syaml.Map(p, "target"); t != nil {
				k.patches = append(k.patches, patchTarget{str(t, "kind"), str(t, "name")})
			}
		}
		k.namespace, k.prefix, k.suffix = lit(kf.Namespace), lit(kf.NamePrefix), lit(kf.NameSuffix)
		for _, img := range kf.Images {
			if s := imageOverride(img); s != "" {
				k.images = append(k.images, s)
			}
		}
		sort.Strings(k.images)
		lines := generatorLines(data)
		for _, gen := range []struct {
			kind    string
			entries []map[string]any
		}{{"ConfigMap", kf.ConfigMapGenerator}, {"Secret", kf.SecretGenerator}} {
			for _, g := range gen.entries {
				name := str(g, "name")
				if name == "" || str(g, "behavior") == "merge" || str(g, "behavior") == "replace" {
					continue
				}
				k.generated = append(k.generated, generatedObj{gen.kind, name, lines[name]})
			}
		}
		out[dir] = k
	}
	return out
}

func lit(s string) string {
	if k8syaml.Literal(s) {
		return s
	}
	return ""
}

func imageOverride(m map[string]any) string {
	name := str(m, "name")
	if name == "" {
		return ""
	}
	to := name
	if n := str(m, "newName"); n != "" {
		to = n
	}
	if d := str(m, "digest"); d != "" {
		return name + "=" + to + "@" + d
	}
	if t := str(m, "newTag"); t != "" {
		return name + "=" + to + ":" + t
	}
	return name + "=" + to
}

// generatorLines finds the line of each generator entry's name, so a generated
// object sits on the line that declares it.
func generatorLines(data []byte) map[string]int {
	out := map[string]int{}
	var root yaml.Node
	if yaml.Unmarshal(data, &root) != nil || len(root.Content) == 0 {
		return out
	}
	top := root.Content[0]
	for i := 0; i+1 < len(top.Content); i += 2 {
		key := top.Content[i].Value
		if key != "configMapGenerator" && key != "secretGenerator" {
			continue
		}
		for _, entry := range top.Content[i+1].Content {
			for j := 0; j+1 < len(entry.Content); j += 2 {
				if entry.Content[j].Value == "name" {
					if _, seen := out[entry.Content[j+1].Value]; !seen {
						out[entry.Content[j+1].Value] = entry.Content[j].Line
					}
				}
			}
		}
	}
	return out
}

// inlinePatchTargets reads the kind and name an inline strategic-merge patch states.
func inlinePatchTargets(s string) []patchTarget {
	var out []patchTarget
	dec := yaml.NewDecoder(strings.NewReader(s))
	for {
		var doc map[string]any
		if dec.Decode(&doc) != nil {
			break
		}
		if kind := str(doc, "kind"); kind != "" {
			if name := k8syaml.String(doc, "metadata", "name"); name != "" {
				out = append(out, patchTarget{kind, name})
			}
		}
	}
	return out
}

// closeBuilds computes each kustomization's closure: the object files it emits,
// directly or through the kustomizations it includes.
func closeBuilds(ks map[string]*kustomization) {
	var visit func(k *kustomization, seen map[string]bool, into map[string]bool)
	visit = func(k *kustomization, seen map[string]bool, into map[string]bool) {
		if seen[k.dir] {
			return
		}
		seen[k.dir] = true
		into[k.file] = true
		for _, f := range k.resources {
			into[f] = true
		}
		for _, d := range k.subdirs {
			if sub := ks[d]; sub != nil {
				visit(sub, seen, into)
			}
		}
	}
	for _, k := range ks {
		k.files = map[string]bool{}
		visit(k, map[string]bool{}, k.files)
		k.size = len(k.files)
	}
}

// buildsHolding lists, for every file, the kustomizations whose closure holds it,
// smallest first.
func buildsHolding(ks map[string]*kustomization) map[string][]*kustomization {
	out := map[string][]*kustomization{}
	for _, k := range ks {
		for f := range k.files {
			out[f] = append(out[f], k)
		}
	}
	for f := range out {
		sort.Slice(out[f], func(i, j int) bool {
			a, b := out[f][i], out[f][j]
			if a.size != b.size {
				return a.size < b.size
			}
			return a.dir < b.dir
		})
	}
	return out
}

// patchFileSet lists every file some kustomization applies as a patch.
func patchFileSet(ks map[string]*kustomization) map[string]bool {
	out := map[string]bool{}
	for _, k := range ks {
		for _, f := range k.patchFiles {
			out[f] = true
		}
	}
	return out
}
