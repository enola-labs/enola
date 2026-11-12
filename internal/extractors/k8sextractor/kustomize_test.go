package k8sextractor

import (
	"reflect"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

const deployment = `apiVersion: apps/v1
kind: Deployment
metadata: {name: api}
spec:
  template:
    metadata: {labels: {app: api}}
    spec:
      containers:
        - name: api
          image: example/api
          envFrom: [{configMapRef: {name: api-env}}]
`

const service = `apiVersion: v1
kind: Service
metadata: {name: api}
spec: {selector: {app: api}}
`

func kustomizeRepo() map[string]string {
	return map[string]string{
		"base/kustomization.yaml": `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - deployment.yaml
  - service.yaml
configMapGenerator:
  - name: api-env
    literals: [LOG_LEVEL=info]
`,
		"base/deployment.yaml": deployment,
		"base/service.yaml":    service,
		"overlays/prod/kustomization.yaml": `apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: prod
namePrefix: prod-
resources:
  - ../../base
  - https://github.com/example/shared//monitoring?ref=v1
components:
  - ../../components/tracing
images:
  - name: example/api
    newTag: "1.4"
patches:
  - path: replicas.yaml
  - target: {kind: Service, name: api}
    patch: |-
      - op: add
        path: /metadata/annotations/a
        value: b
  - patch: |-
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        name: api
      spec:
        template:
          spec:
            serviceAccountName: api
`,
		"overlays/prod/replicas.yaml": `apiVersion: apps/v1
kind: Deployment
metadata: {name: api}
spec: {replicas: 5}
`,
		"components/tracing/kustomization.yaml": `apiVersion: kustomize.config.k8s.io/v1alpha1
kind: Component
resources: [collector.yaml]
`,
		"components/tracing/collector.yaml": `apiVersion: v1
kind: ConfigMap
metadata: {name: collector}
`,
		// A second app declaring the same names: outside every build the names are
		// ambiguous, inside one they are not.
		"other/deployment.yaml": deployment,
		"other/service.yaml":    service,
	}
}

func TestKustomizationIsABuildOfWhatItLists(t *testing.T) {
	ff := extract(t, kustomizeRepo())

	base := find(ff, facts.KindSymbol, "base.Kustomization")
	if base == nil {
		t.Fatal("base kustomization missing")
	}
	if got, want := dependsOn(base), []string{"base.ConfigMap.api-env", "base.Deployment.api", "base.Service.api"}; !reflect.DeepEqual(got, want) {
		t.Errorf("base depends_on = %v, want %v", got, want)
	}
	prod := find(ff, facts.KindSymbol, "overlays/prod.Kustomization")
	if prod == nil {
		t.Fatal("overlay kustomization missing")
	}
	// It builds on the base and the component, and patches the base's Deployment
	// (a patch file and an inline patch) and Service (a JSON 6902 patch by target).
	want := []string{"base.Deployment.api", "base.Kustomization", "base.Service.api", "components/tracing.Component"}
	if got := dependsOn(prod); !reflect.DeepEqual(got, want) {
		t.Errorf("overlay depends_on = %v, want %v", got, want)
	}
	if prod.Props["namespace"] != "prod" || prod.Props["name_prefix"] != "prod-" || prod.Props["patches"] != 3 {
		t.Errorf("overlay props = %v", prod.Props)
	}
	if got := prod.Props["image_overrides"]; !reflect.DeepEqual(got, []string{"example/api=example/api:1.4"}) {
		t.Errorf("image_overrides = %v", got)
	}
	if got := prod.Props["remote_resources"]; !reflect.DeepEqual(got, []string{"https://github.com/example/shared//monitoring?ref=v1"}) {
		t.Errorf("remote_resources = %v", got)
	}
	// A patch is not an object.
	if f := find(ff, facts.KindSymbol, "overlays/prod.Deployment.api"); f != nil {
		t.Error("a patch file became an object")
	}
	// The generated ConfigMap is an object the Deployment names.
	cm := find(ff, facts.KindSymbol, "base.ConfigMap.api-env")
	if cm == nil || cm.Props["generated"] != true || cm.File != "base/kustomization.yaml" || cm.Line != 7 {
		t.Errorf("generated ConfigMap = %+v", cm)
	}
	dep := find(ff, facts.KindSymbol, "base.Deployment.api")
	if got := dependsOn(dep); !reflect.DeepEqual(got, []string{"base.ConfigMap.api-env"}) {
		t.Errorf("Deployment depends_on = %v", got)
	}
	if find(ff, facts.KindModule, "overlays/prod") == nil {
		t.Error("a directory holding only a kustomization is still a module")
	}
}

// A name declared in two apps is ambiguous repository-wide, and unambiguous inside
// the build that holds the referrer.
func TestNamesResolveInsideTheirBuild(t *testing.T) {
	files := kustomizeRepo()
	files["scaling/kustomization.yaml"] = "resources: [../base, hpa.yaml]\n"
	files["scaling/hpa.yaml"] = `apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata: {name: api}
spec: {scaleTargetRef: {kind: Deployment, name: api}}
`
	files["loose/hpa.yaml"] = `apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata: {name: api}
spec: {scaleTargetRef: {kind: Deployment, name: api}}
`
	ff := extract(t, files)
	if got := dependsOn(find(ff, facts.KindSymbol, "scaling.HorizontalPodAutoscaler.api")); !reflect.DeepEqual(got, []string{"base.Deployment.api"}) {
		t.Errorf("HPA inside the build depends_on = %v, want the base Deployment", got)
	}
	if got := dependsOn(find(ff, facts.KindSymbol, "loose.HorizontalPodAutoscaler.api")); got != nil {
		t.Errorf("HPA outside every build depends_on = %v, want nothing: the name is declared twice", got)
	}
}

// A base built by two installs names a Secret each install provides differently:
// one generates it, the other ships a manifest. Both are edges, each stated by a
// build; a third copy outside every build is not.
func TestABaseInSeveralBuildsMeansWhatEachBuildProvides(t *testing.T) {
	ff := extract(t, map[string]string{
		"components/app/kustomization.yaml": "resources: [app.yaml]\n",
		"components/app/app.yaml": `apiVersion: v1
kind: Pod
metadata: {name: app}
spec:
  containers: [{name: a, image: a, envFrom: [{secretRef: {name: db}}]}]
`,
		"components/db/kustomization.yaml":    "resources: [secret.yaml]\n",
		"components/db/secret.yaml":           "apiVersion: v1\nkind: Secret\nmetadata: {name: db}\n",
		"installs/bundled/kustomization.yaml": "resources: [../../components/app, ../../components/db]\n",
		"installs/external/kustomization.yaml": `resources: [../../components/app]
secretGenerator:
  - name: db
    envs: [db.env]
`,
		"stray/secret.yaml": "apiVersion: v1\nkind: Secret\nmetadata: {name: db}\n",
	})
	got := dependsOn(find(ff, facts.KindSymbol, "components/app.Pod.app"))
	want := []string{"components/db.Secret.db", "installs/external.Secret.db"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Pod depends_on = %v, want %v", got, want)
	}
}
