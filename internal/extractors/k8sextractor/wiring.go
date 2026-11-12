package k8sextractor

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/enola-labs/enola/internal/extractors/k8syaml"
	"github.com/enola-labs/enola/internal/factpath"
)

// Workloads find each other by address. microservices-demo's checkout service is
// told where the cart service is with CART_SERVICE_ADDR=cartservice:7070, and that
// string names a Service the repository declares. An env value (or a ConfigMap
// entry a pod reads) that is an address of a declared Service is a dependency on it.
//
// What counts as an address is narrow on purpose, because env values are arbitrary
// text and a Service is often named after a common word: a URL (`http://cart:7070`),
// a `host:port` pair, a gRPC `dns:///host:port` target, or a cluster DNS name ending
// in `.svc` or `.svc.cluster.local`. A bare word never is, even when a Service has
// that name. A name with a second label is read as `<service>.<namespace>` only
// before `.svc`; `cart.example.com` is somebody else's host. When the address states a
// port and the Service declares ports, the port must be one of them.

var (
	schemeAddr = regexp.MustCompile(`^[a-z][a-z0-9+.-]*://(?:[^@/]*@)?([^/:?#\s]+)(?::(\d+))?`)
	grpcAddr   = regexp.MustCompile(`^dns:///?([^/:?#\s]+)(?::(\d+))?$`)
	hostPort   = regexp.MustCompile(`^([a-z0-9](?:[-a-z0-9]*[a-z0-9])?(?:\.[a-z0-9](?:[-a-z0-9]*[a-z0-9])?)*):(\d+)$`)
	svcDNS     = regexp.MustCompile(`^([a-z0-9](?:[-a-z0-9]*[a-z0-9])?(?:\.[a-z0-9](?:[-a-z0-9]*[a-z0-9])?)*\.svc(?:\.cluster\.local)?)\.?$`)
)

// address is a Service an env value names.
type address struct {
	name, namespace string
	port            int // 0 when the value states none
}

// parseAddresses reads the Service addresses in an env value. A value may list
// several, comma-separated (a Kafka bootstrap list).
func parseAddresses(v string) []address {
	var out []address
	for _, part := range strings.Split(v, ",") {
		if a, ok := parseAddress(strings.TrimSpace(part)); ok {
			out = append(out, a)
		}
	}
	return out
}

func parseAddress(v string) (address, bool) {
	if !k8syaml.Literal(v) || v == "" {
		return address{}, false
	}
	host, port := "", ""
	switch {
	case grpcAddr.MatchString(v):
		m := grpcAddr.FindStringSubmatch(v)
		host, port = m[1], m[2]
	case schemeAddr.MatchString(v):
		m := schemeAddr.FindStringSubmatch(v)
		host, port = m[1], m[2]
	case hostPort.MatchString(v):
		m := hostPort.FindStringSubmatch(v)
		host, port = m[1], m[2]
	case svcDNS.MatchString(v):
		host = svcDNS.FindStringSubmatch(v)[1]
	default:
		return address{}, false
	}
	labels := strings.Split(strings.TrimSuffix(host, "."), ".")
	a := address{name: labels[0]}
	switch {
	case len(labels) == 1:
	case len(labels) >= 2 && labels[1] == "svc":
	case len(labels) >= 3 && labels[2] == "svc":
		a.namespace = labels[1]
	default:
		return address{}, false // a host outside the cluster's service names
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil {
			return address{}, false
		}
		a.port = n
	}
	return a, true
}

// servicePorts lists the ports a Service declares.
func servicePorts(doc map[string]any) []int {
	var out []int
	for _, p := range k8syaml.List(doc, "spec", "ports") {
		m, _ := p.(map[string]any)
		if n, ok := m["port"].(int); ok {
			out = append(out, n)
		}
	}
	return out
}

func portAgrees(svc *obj, port int) bool {
	if port == 0 {
		return true
	}
	ports := servicePorts(svc.Doc)
	if len(ports) == 0 {
		return true
	}
	for _, p := range ports {
		if p == port {
			return true
		}
	}
	return false
}

// addressRefs lists the Services the literal addresses in values name.
func (ix *index) addressRefs(from *obj, values []string) []*obj {
	var out []*obj
	for _, v := range values {
		for _, a := range parseAddresses(v) {
			for _, svc := range ix.resolve(from, "Service", a.name, a.namespace) {
				if svc != from && portAgrees(svc, a.port) {
					out = append(out, svc)
				}
			}
		}
	}
	return out
}

// envValues lists the literal env values of a pod spec's containers.
func envValues(spec map[string]any) []string {
	var out []string
	for _, c := range containersOf(spec) {
		env, _ := c["env"].([]any)
		for _, e := range env {
			m, _ := e.(map[string]any)
			if v, ok := m["value"].(string); ok {
				out = append(out, v)
			}
		}
	}
	return out
}

// configMapValues lists a ConfigMap's string data values.
func configMapValues(doc map[string]any) []string {
	var out []string
	data := k8syaml.Map(doc, "data")
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v, ok := data[k].(string); ok && !strings.Contains(v, "\n") {
			out = append(out, v)
		}
	}
	return out
}

// skaffold is the build half of a skaffold.yaml: the images it builds and the
// directory each is built from.
type skaffoldDoc struct {
	APIVersion string        `yaml:"apiVersion"`
	Build      skaffoldBuild `yaml:"build"`
	Profiles   []struct {
		Build skaffoldBuild `yaml:"build"`
	} `yaml:"profiles"`
}

type skaffoldBuild struct {
	Artifacts []struct {
		Image   string `yaml:"image"`
		Context string `yaml:"context"`
	} `yaml:"artifacts"`
}

// buildContexts maps each image a skaffold.yaml builds to the repo-relative
// directory it is built from. An image built from two directories maps to none.
func buildContexts(repoPath string, files []string) map[string]string {
	out := map[string]string{}
	conflict := map[string]bool{}
	for _, rel := range files {
		if base := factpath.Base(rel); base != "skaffold.yaml" && base != "skaffold.yml" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(repoPath, rel))
		if err != nil {
			continue
		}
		dir := factpath.Dir(rel)
		dec := yaml.NewDecoder(bytes.NewReader(data))
		for {
			var doc skaffoldDoc
			err := dec.Decode(&doc)
			if errors.Is(err, io.EOF) || err != nil {
				break
			}
			if !strings.HasPrefix(doc.APIVersion, "skaffold/") {
				continue
			}
			builds := []skaffoldBuild{doc.Build}
			for _, p := range doc.Profiles {
				builds = append(builds, p.Build)
			}
			for _, b := range builds {
				for _, a := range b.Artifacts {
					if !k8syaml.Literal(a.Image) || a.Image == "" {
						continue
					}
					ctx := factpath.Clean(factpath.Join(dir, a.Context))
					if strings.HasPrefix(ctx, "../") {
						continue
					}
					if prev, ok := out[a.Image]; ok && prev != ctx {
						conflict[a.Image] = true
					}
					out[a.Image] = ctx
				}
			}
		}
	}
	for img := range conflict {
		delete(out, img)
	}
	return out
}

// imageName strips a tag or digest: example.io/team/web:1.2@sha256:... is
// example.io/team/web.
func imageName(img string) string {
	if i := strings.Index(img, "@"); i >= 0 {
		img = img[:i]
	}
	slash := strings.LastIndex(img, "/")
	if colon := strings.LastIndex(img, ":"); colon > slash {
		img = img[:colon]
	}
	return img
}

// contextsFor lists the build directories of the images a workload runs. An image
// matches an artifact by name; failing that, an artifact named without a registry
// (`frontend`) matches the last path segment of a pushed image
// (`us-docker.pkg.dev/project/repo/frontend:v1`), which is how skaffold's default
// repository rewrites it, when exactly one artifact has that name.
func contextsFor(imgs []string, contexts map[string]string) []string {
	if len(contexts) == 0 {
		return nil
	}
	bare := map[string][]string{}
	for img := range contexts {
		if !strings.Contains(img, "/") {
			bare[img] = append(bare[img], img)
		}
	}
	set := map[string]bool{}
	for _, img := range imgs {
		name := imageName(img)
		if ctx, ok := contexts[name]; ok {
			set[ctx] = true
			continue
		}
		last := name[strings.LastIndex(name, "/")+1:]
		if b := bare[last]; len(b) == 1 {
			set[contexts[b[0]]] = true
		}
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}
