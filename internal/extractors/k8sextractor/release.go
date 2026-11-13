package k8sextractor

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/enola-labs/enola/internal/extractors/k8syaml"
	"github.com/enola-labs/enola/internal/factpath"
)

// An Application whose chart lives in a registry keeps its configuration in values
// files beside it, and those files are where a GitOps repository says what runs: the
// image, how many replicas, the env, and the addresses of the services it talks to.
// None of them is an object, so without them the Application is a name and a chart.
//
// The values files of an Application are the ones its source lists in
// helm.valueFiles, relative to its path. A plugin source lists none, and reads the
// files in its path by its own convention; then every YAML file there that holds no
// object and has `values` in its name is read, and a file named for one of the
// clusters the Applications sharing that path deploy to (`eu1-values.yaml`) belongs
// to that cluster's Application alone. Files merge in order, later keys winning:
// shared files before a cluster's own, and a file named `override` after the rest.
//
// What is read from the merged values follows the keys `helm create` writes, which
// most charts keep: `image` (a string, or `repository` and `tag`) anywhere in the
// tree, top-level `replicaCount`, `autoscaling.minReplicas`/`maxReplicas`, and
// `resources.requests`/`limits`. Env is every UPPER_SNAKE key holding a scalar, and
// every name/value pair in a list named env. Every string is read for in-cluster
// addresses, by the rules a pod's env values follow.

// release is what an Application's values files state.
type release struct {
	files                          []string
	images                         []string
	replicas, minReplicas, maxReps int
	resources                      []string
	env                            []string
	addresses                      []address
}

// maxEnvValue bounds an env value as recorded; a longer one is cut.
const maxEnvValue = 200

var envKey = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// readReleases reads the values files of every Application deploying a chart that is
// not in its local source path.
func readReleases(repoPath string, files []string, objs []*obj, ks map[string]*kustomization) {
	objFiles := map[string]bool{}
	for _, o := range objs {
		objFiles[o.File] = true
	}
	byDir := map[string][]string{}
	for _, f := range files {
		if !objFiles[f] && !isKustomization(strings.ToLower(factpath.Base(f))) {
			byDir[factpath.Dir(f)] = append(byDir[factpath.Dir(f)], f)
		}
	}
	clusters := map[string]map[string]bool{}
	for _, o := range objs {
		if a := o.app; a != nil && a.local && a.cluster != "" {
			if clusters[a.path] == nil {
				clusters[a.path] = map[string]bool{}
			}
			clusters[a.path][a.cluster] = true
		}
	}
	layers := layerDirs(byDirFiles(byDir))
	for _, o := range objs {
		a := o.app
		if a == nil || !a.local || ks[a.path] != nil || chartAt(repoPath, a.path) {
			continue
		}
		var vf []string
		if len(a.valueFiles) > 0 {
			vf = listedValueFiles(a, files)
		} else {
			vf = append(layeredValueFiles(o, layers, byDir), conventionalValueFiles(a, byDir[a.path], clusters[a.path])...)
		}
		if len(vf) > 0 {
			o.release = readValues(repoPath, vf)
		}
	}
}

func chartAt(repoPath, dir string) bool {
	_, err := os.Stat(filepath.Join(repoPath, filepath.FromSlash(dir), "Chart.yaml"))
	return err == nil
}

// listedValueFiles resolves helm.valueFiles against the source path, keeping the
// ones this repository holds; a `$ref/...` file or a URL is another source's.
func listedValueFiles(a *application, files []string) []string {
	have := map[string]bool{}
	for _, f := range files {
		have[f] = true
	}
	var out []string
	for _, v := range a.valueFiles {
		if strings.HasPrefix(v, "$") || strings.Contains(v, "://") {
			continue
		}
		p := factpath.Clean(factpath.Join(a.path, v))
		if have[p] {
			out = append(out, p)
		}
	}
	return out
}

// conventionalValueFiles picks the values files of a plugin source's path that apply
// to a's cluster, in merge order.
func conventionalValueFiles(a *application, inDir []string, clusters map[string]bool) []string {
	var shared, own []string
	for _, f := range inDir {
		base := factpath.Base(f)
		if !strings.Contains(strings.ToLower(base), "values") {
			continue
		}
		switch c := clusterPrefix(base, clusters); c {
		case "":
			shared = append(shared, f)
		case a.cluster:
			own = append(own, f)
		}
	}
	return append(mergeOrder(shared), mergeOrder(own)...)
}

// clusterPrefix is the longest cluster name a file name starts with, followed by a
// separator, or "".
func clusterPrefix(base string, clusters map[string]bool) string {
	best := ""
	for c := range clusters {
		if len(c) > len(best) && len(base) > len(c) && strings.HasPrefix(base, c) && strings.ContainsRune("-._", rune(base[len(c)])) {
			best = c
		}
	}
	return best
}

// mergeOrder sorts files by name, with any file named override last.
func mergeOrder(fs []string) []string {
	sort.SliceStable(fs, func(i, j int) bool {
		oi := strings.Contains(strings.ToLower(factpath.Base(fs[i])), "override")
		oj := strings.Contains(strings.ToLower(factpath.Base(fs[j])), "override")
		if oi != oj {
			return oj
		}
		return fs[i] < fs[j]
	})
	return fs
}

// readValues merges the values files in order and reads the release they state.
func readValues(repoPath string, files []string) *release {
	merged := map[string]any{}
	r := &release{}
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(repoPath, filepath.FromSlash(f)))
		if err != nil {
			continue
		}
		var doc map[string]any
		if yaml.Unmarshal(data, &doc) != nil || doc == nil {
			continue
		}
		r.files = append(r.files, f)
		deepMerge(merged, doc)
	}
	if len(r.files) == 0 {
		return nil
	}
	r.replicas = intAt(merged, "replicaCount")
	r.minReplicas = intAt(k8syaml.Map(merged, "autoscaling"), "minReplicas")
	r.maxReps = intAt(k8syaml.Map(merged, "autoscaling"), "maxReplicas")
	for _, kind := range []string{"requests", "limits"} {
		res := k8syaml.Map(merged, "resources", kind)
		for _, k := range []string{"cpu", "memory"} {
			if v := scalar(res[k]); v != "" {
				r.resources = append(r.resources, kind+"."+k+"="+v)
			}
		}
	}
	images, env, seenAddr := map[string]bool{}, map[string]bool{}, map[address]bool{}
	walkValues(merged, "", func(key string, v any) {
		if key == "image" {
			if img := imageOf(v); img != "" {
				images[img] = true
			}
		}
		if m, ok := v.(map[string]any); ok {
			for k, val := range m {
				if s := scalar(val); s != "" && envKey.MatchString(k) {
					env[k+"="+clip(s)] = true
				}
			}
		}
		if l, ok := v.([]any); ok && (key == "env" || key == "extraEnv") {
			for _, e := range l {
				m, _ := e.(map[string]any)
				if n, val := str(m, "name"), scalar(m["value"]); n != "" && val != "" {
					env[n+"="+clip(val)] = true
				}
			}
		}
		if s, ok := v.(string); ok {
			for _, a := range parseAddresses(s) {
				if !seenAddr[a] {
					seenAddr[a] = true
					r.addresses = append(r.addresses, a)
				}
			}
		}
	})
	r.images = sortedSet(images)
	r.env = sortedSet(env)
	sort.Slice(r.addresses, func(i, j int) bool { return r.addresses[i].String() < r.addresses[j].String() })
	return r
}

// deepMerge merges src into dst, src winning; maps merge, anything else replaces.
func deepMerge(dst, src map[string]any) {
	for k, v := range src {
		if sm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				deepMerge(dm, sm)
				continue
			}
			cp := map[string]any{}
			deepMerge(cp, sm)
			dst[k] = cp
			continue
		}
		dst[k] = v
	}
}

// walkValues calls visit for every node under its key, depth first, keys sorted.
func walkValues(v any, key string, visit func(key string, v any)) {
	visit(key, v)
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			walkValues(t[k], k, visit)
		}
	case []any:
		for _, e := range t {
			walkValues(e, key, visit)
		}
	}
}

// imageOf reads an image value: a literal string, or a map of repository and tag.
func imageOf(v any) string {
	switch t := v.(type) {
	case string:
		if k8syaml.Literal(t) {
			return t
		}
	case map[string]any:
		repo := str(t, "repository")
		if repo == "" {
			return ""
		}
		if tag := scalar(t["tag"]); tag != "" {
			return repo + ":" + tag
		}
		return repo
	}
	return ""
}

// scalar renders a literal string, number or bool, or "".
func scalar(v any) string {
	switch t := v.(type) {
	case string:
		if k8syaml.Literal(t) {
			return t
		}
	case int, int64, float64, bool:
		return fmt.Sprint(t)
	}
	return ""
}

func intAt(m map[string]any, k string) int {
	n, _ := m[k].(int)
	return n
}

func clip(s string) string {
	if len(s) > maxEnvValue {
		return s[:maxEnvValue]
	}
	return s
}

// String renders an address as the cluster name it states, with its port.
func (a address) String() string {
	s := a.name
	if a.namespace != "" {
		s += "." + a.namespace
	}
	if a.port != 0 {
		s += fmt.Sprintf(":%d", a.port)
	}
	return s
}

// props lists what a release states, as fact props. Only addresses naming a
// namespace are recorded: a bare host:port in a values file is as often a sidecar
// or a local port as a peer.
func (r *release) props() map[string]any {
	out := map[string]any{"value_files": r.files}
	if len(r.images) > 0 {
		out["images"] = r.images
	}
	if r.replicas > 0 {
		out["replicas"] = r.replicas
	}
	if r.minReplicas > 0 {
		out["min_replicas"] = r.minReplicas
	}
	if r.maxReps > 0 {
		out["max_replicas"] = r.maxReps
	}
	if len(r.resources) > 0 {
		out["resources"] = r.resources
	}
	if len(r.env) > 0 {
		out["env"] = r.env
	}
	var addrs []string
	for _, a := range r.addresses {
		if a.namespace != "" {
			addrs = append(addrs, a.String())
		}
	}
	if len(addrs) > 0 {
		out["addresses"] = addrs
	}
	return out
}

// byDirFiles flattens the non-object files by directory back into one list.
func byDirFiles(byDir map[string][]string) []string {
	var out []string
	for _, fs := range byDir {
		out = append(out, fs...)
	}
	return out
}
