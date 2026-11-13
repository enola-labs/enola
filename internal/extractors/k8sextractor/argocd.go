package k8sextractor

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/enola-labs/enola/internal/extractors/k8syaml"
	"github.com/enola-labs/enola/internal/factpath"
)

// An Argo CD Application is a release: it names the cluster and namespace it deploys
// to, the project that governs it, and where its manifests come from, a path in a
// git repository or a chart in a registry. In a GitOps repository the Applications
// are most of what there is, and each one is a bare name unless it carries those
// fields, so they become props on its symbol.
//
// A source names a chart in one of two ways: a Helm source states `chart` and pins it
// with `targetRevision`, and a config-management plugin that resolves the chart
// itself is handed the chart and its version in its env or parameters (CHART_NAME
// and CHART_VERSION, by the convention such plugins share). Both are read.
//
// The Application depends on its AppProject, resolved like any other name, and, when
// its source is a plain path in this repository, on what that path applies: its
// kustomization, or else every object in the directory. A plugin or chart source
// renders the release instead, and the objects beside it, often the Application's
// own siblings for other clusters, are not part of it.

// application is what an Argo CD Application states about the release it deploys.
type application struct {
	cluster      string // destination.name, else destination.server
	namespace    string // destination.namespace
	project      string
	repoURL      string
	path         string // the source path, cleaned, repo-relative; "" for a chart source
	revision     string
	chart        string
	chartVersion string
	plugin       string
	releaseName  string
	valueFiles   []string // helm.valueFiles as written, relative to path
	// local is true when path is a directory of this repository.
	local bool
}

// isApplication reports whether an object is an Argo CD Application.
func isApplication(o *obj) bool {
	return o.Kind == "Application" && strings.HasPrefix(o.APIVersion, "argoproj.io/")
}

// chartEnvNames and chartVersionEnvNames are the env and parameter names through
// which a plugin source is handed the chart it renders.
var (
	chartEnvNames        = map[string]bool{"CHART": true, "CHART_NAME": true, "HELM_CHART": true, "HELM_CHART_NAME": true}
	chartVersionEnvNames = map[string]bool{"CHART_VERSION": true, "HELM_CHART_VERSION": true}
)

// readApplication reads an Application's release, or nil for any other object.
// A multi-source Application (spec.sources) contributes the first source stating
// each field.
func readApplication(o *obj) *application {
	if !isApplication(o) {
		return nil
	}
	spec := k8syaml.Map(o.Doc, "spec")
	a := &application{
		namespace: k8syaml.String(spec, "destination", "namespace"),
		project:   str(spec, "project"),
	}
	if a.cluster = k8syaml.String(spec, "destination", "name"); a.cluster == "" {
		a.cluster = k8syaml.String(spec, "destination", "server")
	}
	var sources []map[string]any
	if s := k8syaml.Map(spec, "source"); s != nil {
		sources = append(sources, s)
	}
	for _, s := range k8syaml.List(spec, "sources") {
		if m, ok := s.(map[string]any); ok {
			sources = append(sources, m)
		}
	}
	for _, s := range sources {
		a.readSource(s)
	}
	return a
}

func (a *application) readSource(s map[string]any) {
	first := func(dst *string, v string) {
		if *dst == "" {
			*dst = v
		}
	}
	if p := str(s, "path"); p != "" && a.path == "" {
		a.path = factpath.Clean(strings.TrimSuffix(p, "/"))
		first(&a.repoURL, str(s, "repoURL"))
		first(&a.revision, str(s, "targetRevision"))
	}
	if c := str(s, "chart"); c != "" && a.chart == "" {
		a.chart = c
		a.chartVersion = str(s, "targetRevision")
		first(&a.repoURL, str(s, "repoURL"))
	}
	helm := k8syaml.Map(s, "helm")
	first(&a.releaseName, str(helm, "releaseName"))
	if len(a.valueFiles) == 0 {
		for _, v := range k8syaml.List(helm, "valueFiles") {
			if f, ok := v.(string); ok && k8syaml.Literal(f) {
				a.valueFiles = append(a.valueFiles, f)
			}
		}
	}
	plugin := k8syaml.Map(s, "plugin")
	if plugin == nil {
		return
	}
	first(&a.plugin, str(plugin, "name"))
	var pairs [][2]string
	for _, e := range k8syaml.List(plugin, "env") {
		if m, ok := e.(map[string]any); ok {
			pairs = append(pairs, [2]string{str(m, "name"), str(m, "value")})
		}
	}
	for _, p := range k8syaml.List(plugin, "parameters") {
		if m, ok := p.(map[string]any); ok {
			pairs = append(pairs, [2]string{str(m, "name"), str(m, "string")})
		}
	}
	for _, p := range pairs {
		name := strings.ToUpper(p[0])
		switch {
		case chartEnvNames[name]:
			first(&a.chart, p[1])
		case chartVersionEnvNames[name]:
			first(&a.chartVersion, p[1])
		}
	}
}

// props lists the fields an Application states, as fact props.
func (a *application) props() map[string]any {
	out := map[string]any{}
	set := func(k, v string) {
		if v != "" {
			out[k] = v
		}
	}
	set("destination_cluster", a.cluster)
	set("destination_namespace", a.namespace)
	set("argocd_project", a.project)
	set("source_repo", a.repoURL)
	set("source_path", a.path)
	set("target_revision", a.revision)
	set("chart", a.chart)
	set("chart_version", a.chartVersion)
	set("source_plugin", a.plugin)
	set("release_name", a.releaseName)
	return out
}

// markLocal decides, for every Application, whether its source path is a directory
// of this repository: the path must exist, and the source repository, when stated,
// must be this one by name (the checkout's directory, or its origin remote).
func markLocal(repoPath string, objs []*obj) {
	names := map[string]bool{strings.ToLower(filepath.Base(repoPath)): true} //factpath:host
	if origin := originRepoName(repoPath); origin != "" {
		names[origin] = true
	}
	for _, o := range objs {
		a := o.app
		if a == nil || a.path == "" || strings.HasPrefix(a.path, "../") {
			continue
		}
		if a.repoURL != "" && !names[repoName(a.repoURL)] {
			continue
		}
		st, err := os.Stat(filepath.Join(repoPath, filepath.FromSlash(a.path)))
		a.local = err == nil && st.IsDir()
	}
}

// repoName is the last path segment of a git URL, without .git, lower-cased:
// git@host:org/shop-deploy.git and https://host/org/shop-deploy are both
// shop-deploy.
func repoName(url string) string {
	url = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(url), "/"), ".git")
	if i := strings.LastIndexAny(url, "/:"); i >= 0 {
		url = url[i+1:]
	}
	return strings.ToLower(url)
}

// originRepoName reads the origin remote's repository name from .git/config, or "".
func originRepoName(repoPath string) string {
	data, err := os.ReadFile(filepath.Join(repoPath, ".git", "config"))
	if err != nil {
		return ""
	}
	inOrigin := false
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			inOrigin = line == `[remote "origin"]`
			continue
		}
		if k, v, ok := strings.Cut(line, "="); inOrigin && ok && strings.TrimSpace(k) == "url" {
			return repoName(v)
		}
	}
	return ""
}

// applicationRefs lists what an Application depends on: its AppProject, and what its
// local source path applies, the kustomization there or else every object there.
func (ix *index) applicationRefs(o *obj, ks map[string]*kustomization) []string {
	a := o.app
	var out []string
	for _, p := range ix.resolve(o, "AppProject", a.project, "") {
		out = append(out, p.symbol)
	}
	if !a.local || a.plugin != "" || a.chart != "" {
		// A plugin or a chart renders the release; the objects beside it are not
		// what it applies.
		return out
	}
	if k := ks[a.path]; k != nil {
		return append(out, k.sym)
	}
	for _, t := range ix.all {
		if t.dir == a.path && t != o {
			out = append(out, t.symbol)
		}
	}
	return out
}

// An address in a release's values names a Service the chart renders, so it is the
// Application deploying that Service that answers, and it may live in another
// repository: the release-addresses binder joins them once every repository is read.
// The extractor states each side: the Application's addresses (props of its
// release), and the names it serves and the environment it deploys to.

// serves lists the Service names an Application deploys: its release name, its own
// name and that name without its cluster appended (`cart-api-eu1` serves
// `cart-api`), and its `app`, `app.kubernetes.io/name` and
// `app.kubernetes.io/instance` labels.
func serves(o *obj) []string {
	a := o.app
	set := map[string]bool{o.Name: true}
	if a.releaseName != "" {
		set[a.releaseName] = true
	}
	if a.cluster != "" && strings.HasSuffix(o.Name, "-"+a.cluster) {
		set[strings.TrimSuffix(o.Name, "-"+a.cluster)] = true
	}
	labels := k8syaml.Labels(k8syaml.Map(o.Doc, "metadata", "labels"))
	for _, k := range []string{"app", "app.kubernetes.io/name", "app.kubernetes.io/instance"} {
		if v := labels[k]; v != "" {
			set[v] = true
		}
	}
	return sortedSet(set)
}

// releaseRefs lists the Services the repository declares that a release's
// addresses name; the Applications serving them are the binder's.
func (ix *index) releaseRefs(o *obj) []string {
	var out []string
	for _, addr := range o.release.addresses {
		ns := addr.namespace
		if ns == "" {
			ns = o.app.namespace
		}
		if ns == "" {
			continue
		}
		in := &obj{Object: k8syaml.Object{Namespace: ns}, dir: o.dir}
		for _, svc := range ix.resolve(in, "Service", addr.name, ns) {
			if portAgrees(svc, addr.port) {
				out = append(out, svc.symbol)
			}
		}
	}
	return out
}

// environments maps the words naming an environment to one spelling each.
var environments = map[string]string{
	"production": "production", "prod": "production", "sandbox": "sandbox",
	"staging": "staging", "stage": "staging", "development": "development",
	"dev": "development", "test": "test", "qa": "qa",
}

// environment is the environment an Application deploys to: its `environment` or
// `env` label, else the one environment word among its project name's parts, or "".
func environment(o *obj) string {
	labels := k8syaml.Labels(k8syaml.Map(o.Doc, "metadata", "labels"))
	for _, k := range []string{"environment", "env"} {
		if v := strings.ToLower(labels[k]); v != "" {
			if e, ok := environments[v]; ok {
				return e
			}
			return v
		}
	}
	found := ""
	for _, p := range strings.Split(strings.ToLower(o.app.project), "-") {
		if e, ok := environments[p]; ok {
			if found != "" && found != e {
				return ""
			}
			found = e
		}
	}
	return found
}
