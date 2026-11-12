package k8sextractor

import (
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/extractors/k8syaml"
)

// clusterScoped are the kinds no namespace qualifies, so a namespaced referrer
// reaches them whatever its own namespace.
var clusterScoped = map[string]bool{
	"Namespace": true, "Node": true, "PersistentVolume": true, "StorageClass": true,
	"ClusterRole": true, "ClusterRoleBinding": true, "CustomResourceDefinition": true,
	"IngressClass": true, "PriorityClass": true, "GatewayClass": true, "APIService": true,
	"MutatingWebhookConfiguration": true, "ValidatingWebhookConfiguration": true,
	"RuntimeClass": true, "CSIDriver": true, "VolumeSnapshotClass": true,
}

// index finds objects by kind and name.
type index struct {
	byKindName map[string][]*obj
	workloads  []*obj
}

func newIndex(objs []*obj) *index {
	ix := &index{byKindName: map[string][]*obj{}}
	for _, o := range objs {
		k := o.Kind + "\x00" + o.Name
		ix.byKindName[k] = append(ix.byKindName[k], o)
		if podTemplate(o.Object) != nil {
			ix.workloads = append(ix.workloads, o)
		}
	}
	return ix
}

func nsAgree(from *obj, to *obj, ns string) bool {
	if clusterScoped[to.Kind] {
		return true
	}
	if ns == "" {
		ns = from.Namespace
	}
	return ns == "" || to.Namespace == "" || ns == to.Namespace
}

// resolve finds the object of kind and name a reference from `from` means: the one
// in from's directory, else the one in the repository, else none. ns is the
// namespace the reference states, if any.
func (ix *index) resolve(from *obj, kind, name, ns string) *obj {
	if !k8syaml.Literal(name) {
		return nil
	}
	var local, all []*obj
	for _, c := range ix.byKindName[kind+"\x00"+name] {
		if !nsAgree(from, c, ns) {
			continue
		}
		all = append(all, c)
		if c.dir == from.dir {
			local = append(local, c)
		}
	}
	if len(local) == 1 {
		return local[0]
	}
	if len(local) == 0 && len(all) == 1 {
		return all[0]
	}
	return nil
}

// selectWorkloads finds the workloads whose pod labels carry every label in sel: the
// ones in from's directory, else the one in the repository.
func (ix *index) selectWorkloads(from *obj, sel map[string]string) []*obj {
	if len(sel) == 0 {
		return nil
	}
	var local, all []*obj
	for _, w := range ix.workloads {
		if w == from || !nsAgree(from, w, "") {
			continue
		}
		if !labelsMatch(podLabels(w.Object), sel) {
			continue
		}
		all = append(all, w)
		if w.dir == from.dir {
			local = append(local, w)
		}
	}
	if len(local) > 0 {
		return local
	}
	if len(all) == 1 {
		return all
	}
	return nil
}

func labelsMatch(labels, sel map[string]string) bool {
	for k, v := range sel {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// references lists the fact names of the objects o names.
func (ix *index) references(o *obj) []string {
	var out []string
	add := func(t *obj) {
		if t != nil {
			out = append(out, t.symbol)
		}
	}
	doc := o.Doc
	switch o.Kind {
	case "Service":
		if k8syaml.String(doc, "spec", "type") != "ExternalName" {
			for _, w := range ix.selectWorkloads(o, k8syaml.Labels(k8syaml.Map(doc, "spec", "selector"))) {
				add(w)
			}
		}
	case "Ingress":
		for _, name := range ingressBackends(doc) {
			add(ix.resolve(o, "Service", name, ""))
		}
		add(ix.resolve(o, "IngressClass", k8syaml.String(doc, "spec", "ingressClassName"), ""))
	case "HTTPRoute", "GRPCRoute", "TCPRoute", "TLSRoute", "UDPRoute":
		for _, p := range k8syaml.List(doc, "spec", "parentRefs") {
			if m, ok := p.(map[string]any); ok {
				add(ix.resolve(o, kindOr(m, "Gateway"), str(m, "name"), str(m, "namespace")))
			}
		}
		for _, r := range k8syaml.List(doc, "spec", "rules") {
			rule, ok := r.(map[string]any)
			if !ok {
				continue
			}
			refs, _ := rule["backendRefs"].([]any)
			for _, b := range refs {
				if m, ok := b.(map[string]any); ok {
					add(ix.resolve(o, kindOr(m, "Service"), str(m, "name"), str(m, "namespace")))
				}
			}
		}
	case "Gateway":
		add(ix.resolve(o, "GatewayClass", k8syaml.String(doc, "spec", "gatewayClassName"), ""))
	case "RoleBinding", "ClusterRoleBinding":
		if rr := k8syaml.Map(doc, "roleRef"); rr != nil {
			add(ix.resolve(o, str(rr, "kind"), str(rr, "name"), ""))
		}
		for _, s := range k8syaml.List(doc, "subjects") {
			if m, ok := s.(map[string]any); ok && str(m, "kind") == "ServiceAccount" {
				add(ix.resolve(o, "ServiceAccount", str(m, "name"), str(m, "namespace")))
			}
		}
	case "HorizontalPodAutoscaler":
		if t := k8syaml.Map(doc, "spec", "scaleTargetRef"); t != nil {
			add(ix.resolve(o, str(t, "kind"), str(t, "name"), ""))
		}
	case "VerticalPodAutoscaler":
		if t := k8syaml.Map(doc, "spec", "targetRef"); t != nil {
			add(ix.resolve(o, str(t, "kind"), str(t, "name"), ""))
		}
	case "PodDisruptionBudget":
		for _, w := range ix.selectWorkloads(o, k8syaml.Labels(k8syaml.Map(doc, "spec", "selector", "matchLabels"))) {
			add(w)
		}
	case "PersistentVolumeClaim":
		add(ix.resolve(o, "StorageClass", k8syaml.String(doc, "spec", "storageClassName"), ""))
	}
	if spec := podSpec(o.Object); spec != nil {
		for _, r := range podSpecRefs(spec) {
			add(ix.resolve(o, r.kind, r.name, ""))
		}
	}
	return out
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	if !k8syaml.Literal(s) {
		return ""
	}
	return s
}

func kindOr(m map[string]any, def string) string {
	if k := str(m, "kind"); k != "" {
		return k
	}
	return def
}

// podTemplate returns the pod template of a workload: a Pod's own document, a
// CronJob's job template's pod template, or any object's spec.template carrying
// containers (Deployment, StatefulSet, DaemonSet, Job, ReplicaSet, and CRDs shaped
// like them, such as an Argo Rollout).
func podTemplate(o k8syaml.Object) map[string]any {
	switch o.Kind {
	case "Pod":
		return o.Doc
	case "CronJob":
		return k8syaml.Map(o.Doc, "spec", "jobTemplate", "spec", "template")
	}
	t := k8syaml.Map(o.Doc, "spec", "template")
	if t == nil || k8syaml.List(t, "spec", "containers") == nil {
		return nil
	}
	return t
}

func podSpec(o k8syaml.Object) map[string]any {
	t := podTemplate(o)
	if t == nil {
		return nil
	}
	return k8syaml.Map(t, "spec")
}

func podLabels(o k8syaml.Object) map[string]string {
	t := podTemplate(o)
	if t == nil {
		return nil
	}
	return k8syaml.Labels(k8syaml.Map(t, "metadata", "labels"))
}

type ref struct{ kind, name string }

func containersOf(spec map[string]any) []map[string]any {
	var out []map[string]any
	for _, key := range []string{"initContainers", "containers", "ephemeralContainers"} {
		l, _ := spec[key].([]any)
		for _, c := range l {
			if m, ok := c.(map[string]any); ok {
				out = append(out, m)
			}
		}
	}
	return out
}

// podSpecRefs lists the names a pod spec mounts or reads.
func podSpecRefs(spec map[string]any) []ref {
	var out []ref
	if sa := str(spec, "serviceAccountName"); sa != "" {
		out = append(out, ref{"ServiceAccount", sa})
	} else if sa := str(spec, "serviceAccount"); sa != "" {
		out = append(out, ref{"ServiceAccount", sa})
	}
	add := func(kind string, m map[string]any, key string) {
		if m != nil {
			if n := str(m, key); n != "" {
				out = append(out, ref{kind, n})
			}
		}
	}
	for _, c := range containersOf(spec) {
		envFrom, _ := c["envFrom"].([]any)
		for _, e := range envFrom {
			m, _ := e.(map[string]any)
			add("ConfigMap", k8syaml.Map(m, "configMapRef"), "name")
			add("Secret", k8syaml.Map(m, "secretRef"), "name")
		}
		env, _ := c["env"].([]any)
		for _, e := range env {
			m, _ := e.(map[string]any)
			add("ConfigMap", k8syaml.Map(m, "valueFrom", "configMapKeyRef"), "name")
			add("Secret", k8syaml.Map(m, "valueFrom", "secretKeyRef"), "name")
		}
	}
	vols, _ := spec["volumes"].([]any)
	for _, v := range vols {
		m, _ := v.(map[string]any)
		add("ConfigMap", k8syaml.Map(m, "configMap"), "name")
		add("Secret", k8syaml.Map(m, "secret"), "secretName")
		add("PersistentVolumeClaim", k8syaml.Map(m, "persistentVolumeClaim"), "claimName")
		sources := k8syaml.List(m, "projected", "sources")
		for _, s := range sources {
			sm, _ := s.(map[string]any)
			add("ConfigMap", k8syaml.Map(sm, "configMap"), "name")
			add("Secret", k8syaml.Map(sm, "secret"), "name")
		}
	}
	pulls, _ := spec["imagePullSecrets"].([]any)
	for _, p := range pulls {
		m, _ := p.(map[string]any)
		add("Secret", m, "name")
	}
	return out
}

// images lists the literal container images a workload runs, sorted.
func images(doc map[string]any) []string {
	spec := podSpec(k8syaml.Object{Kind: kindOf(doc), Doc: doc})
	if spec == nil {
		return nil
	}
	set := map[string]bool{}
	for _, c := range containersOf(spec) {
		if img := str(c, "image"); img != "" {
			set[img] = true
		}
	}
	out := make([]string, 0, len(set))
	for i := range set {
		out = append(out, i)
	}
	sort.Strings(out)
	return out
}

func kindOf(doc map[string]any) string {
	k, _ := doc["kind"].(string)
	return k
}

// ingressBackends lists the Service names an Ingress sends to, in either API shape
// (networking.k8s.io/v1 backend.service.name, v1beta1 backend.serviceName).
func ingressBackends(doc map[string]any) []string {
	var out []string
	backend := func(b map[string]any) {
		if b == nil {
			return
		}
		if n := k8syaml.String(b, "service", "name"); n != "" {
			out = append(out, n)
		} else if n := str(b, "serviceName"); n != "" {
			out = append(out, n)
		}
	}
	backend(k8syaml.Map(doc, "spec", "defaultBackend"))
	backend(k8syaml.Map(doc, "spec", "backend"))
	for _, r := range k8syaml.List(doc, "spec", "rules") {
		rule, _ := r.(map[string]any)
		for _, p := range k8syaml.List(rule, "http", "paths") {
			pm, _ := p.(map[string]any)
			backend(k8syaml.Map(pm, "backend"))
		}
	}
	return out
}

// ingressPaths lists an Ingress's host+path pairs, sorted. They are recorded on the
// Ingress and never become route facts: an ingress prefix is not an endpoint, and the
// HTTP linker would match clients against it.
func ingressPaths(o *obj) []string {
	if o.Kind != "Ingress" {
		return nil
	}
	set := map[string]bool{}
	for _, r := range k8syaml.List(o.Doc, "spec", "rules") {
		rule, _ := r.(map[string]any)
		host := str(rule, "host")
		for _, p := range k8syaml.List(rule, "http", "paths") {
			pm, _ := p.(map[string]any)
			path := str(pm, "path")
			if path == "" {
				path = "/"
			}
			set[strings.TrimSpace(host+path)] = true
		}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}
