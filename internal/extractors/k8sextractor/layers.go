package k8sextractor

import (
	"regexp"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
)

// Some plugins layer shared values under an Application's own: a directory named
// for values beside an ancestor of its path (`shop/_shared_values/` above
// `shop/web/` and `shop/worker/`) holds files that apply to some of the
// Applications below, chosen by name. Nothing in the Application names them; the
// file names do, by convention:
//
//	01-global.yaml            every Application
//	20-production.yaml        those of the production environment
//	30-eu1.yaml               those in region eu1
//	40-eu1prod1.yaml          those on cluster eu1prod1
//	50-web.yaml               the web workload
//	60-web-production.yaml    the web workload in production
//
// A layered file named global, common, default(s), all, base or shared applies to
// every Application. Any other applies when its name, without the leading number, splits wholly
// into things the Application states: its cluster, its region (the cluster name's
// leading letters and digits), its workload (the source path's last directory, or its
// path below the layered directory's parent joined by dashes), its labels' values,
// its project and each dash-separated part of it, its namespace, its own name and
// release name. A part one of those cannot account for excludes the file, so
// `50-web.yaml` is not `web-admin`'s, and `60-web-sandbox.yaml` is not production's.
// Layered files merge in name order, before the Application's own values.

// everyone are the names a layered file uses to apply to every Application.
var everyone = map[string]bool{
	"global": true, "common": true, "default": true, "defaults": true, "all": true,
	"base": true, "shared": true,
}

var (
	layerNumber  = regexp.MustCompile(`^[0-9]+-`)
	regionPrefix = regexp.MustCompile(`^[a-z]+[0-9]+`)
)

// layerDirs maps each directory to the values directories directly inside it.
func layerDirs(files []string) map[string][]string {
	out := map[string][]string{}
	seen := map[string]bool{}
	for _, f := range files {
		d := factpath.Dir(f)
		if d == "." || seen[d] || !strings.Contains(strings.ToLower(factpath.Base(d)), "values") {
			continue
		}
		seen[d] = true
		parent := factpath.Dir(d)
		out[parent] = append(out[parent], d)
	}
	return out
}

// layeredValueFiles lists the layered files applying to an Application, in merge
// order: those of the nearest ancestor holding a values directory.
func layeredValueFiles(o *obj, layers map[string][]string, byDir map[string][]string) []string {
	a := o.app
	for base := factpath.Dir(a.path); ; base = factpath.Dir(base) {
		if dirs := layers[base]; len(dirs) > 0 {
			facets := applicationFacets(o, base)
			var out []string
			for _, d := range dirs {
				if strings.HasPrefix(a.path+"/", d+"/") {
					continue
				}
				for _, f := range byDir[d] {
					if layerApplies(factpath.Base(f), facets) {
						out = append(out, f)
					}
				}
			}
			sort.Slice(out, func(i, j int) bool {
				if bi, bj := factpath.Base(out[i]), factpath.Base(out[j]); bi != bj {
					return bi < bj
				}
				return out[i] < out[j]
			})
			return out
		}
		if base == "." || base == "" {
			return nil
		}
	}
}

// applicationFacets are the names an Application answers to, for layered files.
func applicationFacets(o *obj, base string) map[string]bool {
	a := o.app
	out := map[string]bool{}
	add := func(s string) {
		if s != "" {
			out[s] = true
		}
	}
	add(a.cluster)
	add(regionPrefix.FindString(a.cluster))
	add(factpath.Base(a.path))
	rel := a.path
	if base != "." {
		rel = strings.TrimPrefix(a.path, base+"/")
	}
	add(strings.ReplaceAll(rel, "/", "-"))
	add(a.namespace)
	add(a.releaseName)
	add(o.Name)
	add(a.project)
	for _, p := range strings.Split(a.project, "-") {
		add(p)
	}
	for _, v := range labelValues(o) {
		add(v)
	}
	return out
}

func labelValues(o *obj) []string {
	var out []string
	if m, ok := o.Doc["metadata"].(map[string]any); ok {
		if l, ok := m["labels"].(map[string]any); ok {
			for _, v := range l {
				if s, ok := v.(string); ok {
					out = append(out, s)
				}
			}
		}
	}
	return out
}

// layerApplies reports whether a layered file's name splits wholly into facets.
func layerApplies(base string, facets map[string]bool) bool {
	stem := strings.TrimSuffix(strings.TrimSuffix(base, ".yaml"), ".yml")
	stem = layerNumber.ReplaceAllString(stem, "")
	if stem == "" {
		return false
	}
	if everyone[stem] {
		return true
	}
	parts := strings.Split(stem, "-")
	// ok[i]: parts[i:] split wholly into facets.
	ok := make([]bool, len(parts)+1)
	ok[len(parts)] = true
	for i := len(parts) - 1; i >= 0; i-- {
		for j := i + 1; j <= len(parts) && !ok[i]; j++ {
			ok[i] = ok[j] && facets[strings.Join(parts[i:j], "-")]
		}
	}
	return ok[0]
}
