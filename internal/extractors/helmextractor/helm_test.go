package helmextractor

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func extract(t *testing.T, files map[string]string) []facts.Fact {
	t.Helper()
	dir := t.TempDir()
	for f, c := range files {
		p := filepath.Join(dir, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ff, err := New().Extract(context.Background(), dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ff
}

func find(ff []facts.Fact, name string) *facts.Fact {
	for i := range ff {
		if ff[i].Kind == facts.KindSymbol && ff[i].Name == name {
			return &ff[i]
		}
	}
	return nil
}

func targets(f *facts.Fact, kind string) []string {
	if f == nil {
		return nil
	}
	var out []string
	for _, r := range f.Relations {
		if r.Kind == kind {
			out = append(out, r.Target)
		}
	}
	return out
}

func chartRepo() map[string]string {
	return map[string]string{
		"charts/common/Chart.yaml": "apiVersion: v2\nname: common\nversion: 2.31.4\ntype: library\n",
		"charts/common/templates/_names.tpl": `{{- define "common.names.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 -}}
{{- else -}}
{{- include "common.names.name" . -}}
{{- end -}}
{{- end -}}

{{- define "common.names.name" -}}{{ .Chart.Name }}{{- end -}}
`,
		"charts/web/Chart.yaml": `apiVersion: v2
name: web
version: 1.0.0
appVersion: "2.4"
dependencies:
  - name: common
    version: 2.x.x
    repository: oci://registry-1.docker.io/bitnamicharts
  - name: postgresql
    version: 16.x.x
    repository: oci://registry-1.docker.io/bitnamicharts
    condition: postgresql.enabled
  - name: worker
    version: 0.1.0
    repository: file://../worker
`,
		"charts/web/templates/_helpers.tpl": `{{- define "web.labels" -}}
app: {{ include "common.names.name" . }}
{{- end -}}
`,
		"charts/web/templates/deployment.yaml": `{{- if .Values.enabled }}
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ include "common.names.fullname" . }}
  labels: {{- include "web.labels" . | nindent 4 }}
spec:
  template:
    spec:
      containers:
        - name: web
          image: nginx
---
apiVersion: v1
kind: Service
metadata:
  name: web-svc
data:
  conf: |
    # a config file that says template "x" is not a call
    upstream template "not.a.call"
{{- end }}
`,
		"charts/web/templates/jobs/migrate.yaml": `apiVersion: batch/v1
kind: Job
metadata:
  name: {{ include "missing.template" . }}
`,
		"charts/web/templates/NOTES.txt": `{{ include "web.labels" . }}`,
		"charts/web/crds/widgets.yaml": `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: widgets.example.com
`,
		"charts/web/charts/cache/Chart.yaml":                "apiVersion: v2\nname: cache\nversion: 0.2.0\n",
		"charts/web/charts/cache/templates/_h.tpl":          `{{ define "cache.name" }}cache{{ end }}`,
		"charts/worker/Chart.yaml":                          "apiVersion: v2\nname: worker\nversion: 0.1.0\n",
		"charts/postgresql/Chart.yaml":                      "apiVersion: v2\nname: postgresql\nversion: 17.1.2\n",
		"charts/postgresql/templates/primary/svc.yaml":      "apiVersion: v1\nkind: Service\nmetadata:\n  name: {{ .Release.Name }}\n",
		"charts/postgresql/templates/readreplicas/svc.yaml": "apiVersion: v1\nkind: Service\nmetadata:\n  name: {{ .Release.Name }}-read\n",
	}
}

func TestChartsAndWhatTheyLoad(t *testing.T) {
	ff := extract(t, chartRepo())
	web := find(ff, "charts/web.Chart.web")
	if web == nil {
		t.Fatal("chart missing")
	}
	if web.Props["chart_type"] != "application" || web.Props["version"] != "1.0.0" || web.Props["app_version"] != "2.4" {
		t.Errorf("chart props = %v", web.Props)
	}
	want := []string{
		"charts/common.Chart.common", // by name, 2.31.4 satisfies 2.x.x
		"charts/web.CustomResourceDefinition.widgets.example.com",
		"charts/web.Deployment.deployment",
		"charts/web.Job.jobs/migrate",
		"charts/web.Service.web-svc",
		"charts/web/charts/cache.Chart.cache", // a subchart, loaded whether listed or not
		"charts/worker.Chart.worker",          // file://
	}
	if got := targets(web, facts.RelDependsOn); !reflect.DeepEqual(got, want) {
		t.Errorf("chart depends_on =\n %v\nwant\n %v", got, want)
	}
	// postgresql 16.x.x is not the repository's 17.1.2: it comes from the registry.
	if got := web.Props["remote_dependencies"]; !reflect.DeepEqual(got, []string{"postgresql"}) {
		t.Errorf("remote_dependencies = %v", got)
	}
	if got := web.Props["dependencies_by_name"]; !reflect.DeepEqual(got, []string{"common"}) {
		t.Errorf("dependencies_by_name = %v", got)
	}
	if c := find(ff, "charts/common.Chart.common"); c == nil || c.Props["chart_type"] != "library" {
		t.Errorf("library chart = %+v", c)
	}
}

func TestIncludesResolveAcrossLoadedCharts(t *testing.T) {
	ff := extract(t, chartRepo())
	dep := find(ff, "charts/web.Deployment.deployment")
	if dep == nil {
		t.Fatal("Deployment missing")
	}
	if dep.Line != 2 || dep.Props["k8s_kind"] != "Deployment" || dep.Props["api_version"] != "apps/v1" {
		t.Errorf("Deployment = %+v", dep)
	}
	want := []string{"charts/common.template.common.names.fullname", "charts/web.template.web.labels"}
	if got := targets(dep, facts.RelCalls); !reflect.DeepEqual(got, want) {
		t.Errorf("Deployment calls = %v, want %v", got, want)
	}
	// The include inside a define, past nested if/else blocks, belongs to the define.
	full := find(ff, "charts/common.template.common.names.fullname")
	if got := targets(full, facts.RelCalls); !reflect.DeepEqual(got, []string{"charts/common.template.common.names.name"}) {
		t.Errorf("fullname calls = %v", got)
	}
	if full.Props["helm_kind"] != "named_template" || full.Props["symbol_kind"] != "function" || full.Line != 1 {
		t.Errorf("define = %+v", full)
	}
	if name := find(ff, "charts/common.template.common.names.name"); name == nil || name.Line != 9 {
		t.Errorf("one-line define = %+v", name)
	}
	// A literal metadata.name names the object; text that says template is no call.
	svc := find(ff, "charts/web.Service.web-svc")
	if svc == nil || len(targets(svc, facts.RelCalls)) != 0 || svc.Props["unresolved_includes"] != nil {
		t.Errorf("Service = %+v", svc)
	}
	// A name no loaded chart defines is counted, never guessed.
	job := find(ff, "charts/web.Job.jobs/migrate")
	if job == nil || job.Props["unresolved_includes"] != 1 {
		t.Errorf("Job = %+v", job)
	}
	// Templates in different directories with one file name stay distinct.
	for _, n := range []string{"charts/postgresql.Service.primary/svc", "charts/postgresql.Service.readreplicas/svc"} {
		if find(ff, n) == nil {
			t.Errorf("%s missing", n)
		}
	}
	if crd := find(ff, "charts/web.CustomResourceDefinition.widgets.example.com"); crd == nil || crd.Props["helm_kind"] != "crd" {
		t.Errorf("CRD = %+v", crd)
	}
}

// Two loaded charts defining one name leave an include of it unresolved.
func TestAnAmbiguousIncludeDrawsNothing(t *testing.T) {
	ff := extract(t, map[string]string{
		"app/Chart.yaml":               "apiVersion: v2\nname: app\nversion: 1.0.0\n",
		"app/templates/cm.yaml":        "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ include \"shared.name\" . }}\n",
		"app/charts/a/Chart.yaml":      "apiVersion: v2\nname: a\nversion: 1.0.0\n",
		"app/charts/a/templates/_.tpl": `{{ define "shared.name" }}a{{ end }}`,
		"app/charts/b/Chart.yaml":      "apiVersion: v2\nname: b\nversion: 1.0.0\n",
		"app/charts/b/templates/_.tpl": `{{ define "shared.name" }}b{{ end }}`,
	})
	cm := find(ff, "app.ConfigMap.cm")
	if got := targets(cm, facts.RelCalls); got != nil || cm.Props["unresolved_includes"] != 1 {
		t.Errorf("ConfigMap calls = %v, props %v", got, cm.Props)
	}
}

func TestDetectNeedsAChart(t *testing.T) {
	if ok, _ := New().DetectFiles("", []string{"deploy/app.yaml", ".github/x/Chart.yaml"}); ok {
		t.Error("detected without a chart")
	}
	if ok, _ := New().DetectFiles("", []string{"helm/app/Chart.yaml"}); !ok {
		t.Error("missed a chart")
	}
}

func TestSatisfies(t *testing.T) {
	cases := []struct {
		version, constraint string
		want                bool
	}{
		{"2.31.4", "2.x.x", true},
		{"2.31.4", "2.X.X", true},
		{"17.1.2", "16.x.x", false},
		{"1.2.3", "1.2.3", true},
		{"1.2.4", "1.2.3", false},
		{"1.4.0", "^1.2.0", true},
		{"2.0.0", "^1.2.0", false},
		{"0.2.5", "^0.2.1", true},
		{"0.3.0", "^0.2.1", false},
		{"1.2.9", "~1.2.3", true},
		{"1.3.0", "~1.2.3", false},
		{"5.0.0", ">=4.0.0 <6.0.0", true},
		{"6.0.0", ">=4.0.0, <6.0.0", false},
		{"3.0.0", "1.x || 3.x", true},
		{"1.5.0", "1.0.0 - 2.0.0", true},
		{"v1.2.3-rc.1", "1.2.x", true},
		{"1.2.3", "", true},
		{"1.2.3", "latest", false},
		{"not-a-version", "1.x", false},
	}
	for _, c := range cases {
		if got := satisfies(c.version, c.constraint); got != c.want {
			t.Errorf("satisfies(%q, %q) = %v, want %v", c.version, c.constraint, got, c.want)
		}
	}
}

func valuesRepo() map[string]string {
	return map[string]string{
		"lib/Chart.yaml": "apiVersion: v2\nname: lib\nversion: 1.0.0\ntype: library\n",
		"lib/templates/_names.tpl": `{{- define "lib.fullname" -}}
{{- default .Chart.Name .Values.fullnameOverride -}}
{{- end -}}
{{- define "lib.image" -}}
{{ include "lib.registry" . }}/{{ .imageRoot.repository }}
{{- end -}}
{{- define "lib.registry" -}}{{ .Values.global.imageRegistry }}{{- end -}}
`,
		"app/Chart.yaml": `apiVersion: v2
name: app
version: 1.0.0
dependencies:
  - name: lib
    version: 1.x.x
    repository: file://../lib
  - name: redis
    version: 19.x.x
    repository: oci://registry.example.com/charts
    alias: cache
`,
		"app/values.yaml": `fullnameOverride: ""
image:
  registry: docker.io
  repository: example/app
  tag: "1.0"
service:
  port: 80
  annotations:
    prometheus.io/scrape: "true"
persistence:
  enabled: false
  size: 8Gi
unusedSetting: true
debug:
  verbose: false
  level: info
global:
  imageRegistry: ""
cache:
  enabled: true
`,
		"app/templates/deployment.yaml": `apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ include "lib.fullname" . }}
spec:
  template:
    spec:
      containers:
        - name: app
          image: {{ include "lib.image" (dict "imageRoot" .Values.image "global" .Values.global) }}
          {{- with .Values.persistence }}
          volumeMounts: {{ .enabled }}
          {{- end }}
          env:
            - name: LEVEL
              value: {{ index .Values "debug" "level" | quote }}
`,
		"app/templates/service.yaml": `apiVersion: v1
kind: Service
metadata:
  name: app
  annotations: {{- toYaml $.Values.service.annotations | nindent 4 }}
spec:
  ports:
    - port: {{ .Values.service.port }}
`,
		"app/templates/NOTES.txt": `Visit port {{ .Values.service.port }}.
{{ include "app.notes.extra" . }}`,
		"app/templates/_helpers.tpl": `{{- define "app.notes.extra" -}}
debug={{ .Values.debug.verbose }}
{{- end -}}
{{- define "app.unused" -}}x{{- end -}}
`,
	}
}

func TestValuesAreSymbolsNamedByTheirReaders(t *testing.T) {
	ff := extract(t, valuesRepo())
	// Declared keys two levels deep become symbols; global and the aliased
	// dependency's key are values for other charts, not this one.
	for _, n := range []string{"app.values.image", "app.values.image.tag", "app.values.unusedSetting"} {
		if find(ff, n) == nil {
			t.Errorf("%s missing", n)
		}
	}
	if find(ff, "app.values.service.annotations.prometheus.io/scrape") != nil {
		t.Error("a key three levels deep became a symbol")
	}
	for _, n := range []string{"app.values.global", "app.values.global.imageRegistry", "app.values.cache", "app.values.cache.enabled"} {
		if find(ff, n) != nil {
			t.Errorf("%s is passed to another chart and must not be a value of this one", n)
		}
	}
	tag := find(ff, "app.values.image.tag")
	if tag == nil || tag.Props["helm_kind"] != "value" || tag.Props["symbol_kind"] != "variable" || tag.Line != 5 || tag.File != "app/values.yaml" {
		t.Fatalf("image.tag = %+v", tag)
	}

	dep := find(ff, "app.Deployment.deployment")
	want := []string{
		"app.values.debug", "app.values.debug.level", // index .Values "debug" "level"
		"app.values.fullnameOverride",                                                                          // read by lib.fullname on this chart's behalf
		"app.values.image", "app.values.image.registry", "app.values.image.repository", "app.values.image.tag", // passed whole
		"app.values.persistence", "app.values.persistence.enabled", "app.values.persistence.size", // with .Values.persistence
	}
	if got := targets(dep, facts.RelNames); !reflect.DeepEqual(got, want) {
		t.Errorf("Deployment names =\n %v\nwant\n %v", got, want)
	}
	// The library's define reads no value of its own chart and draws no edge into app.
	if got := targets(find(ff, "lib.template.lib.fullname"), facts.RelNames); got != nil {
		t.Errorf("lib.fullname names = %v, want nothing: its reads belong to the chart including it", got)
	}
	svc := find(ff, "app.Service.app")
	if got := targets(svc, facts.RelNames); !reflect.DeepEqual(got, []string{"app.values.service", "app.values.service.annotations", "app.values.service.port"}) {
		t.Errorf("Service names = %v", got)
	}
	// NOTES.txt is no resource: its calls and reads land on a file_ref.
	var notes *facts.Fact
	for i := range ff {
		if ff[i].Kind == facts.KindFileRef && ff[i].Name == "app/templates/NOTES.txt" {
			notes = &ff[i]
		}
	}
	if notes == nil {
		t.Fatal("NOTES.txt file_ref missing")
	}
	if got := targets(notes, facts.RelCalls); !reflect.DeepEqual(got, []string{"app.template.app.notes.extra"}) {
		t.Errorf("NOTES.txt calls = %v", got)
	}
	if got := targets(find(ff, "app.template.app.notes.extra"), facts.RelNames); !reflect.DeepEqual(got, []string{"app.values.debug", "app.values.debug.verbose"}) {
		t.Errorf("notes define names = %v", got)
	}
	// unusedSetting has no reader at all.
	for _, f := range ff {
		for _, r := range f.Relations {
			if r.Target == "app.values.unusedSetting" {
				t.Errorf("%s names unusedSetting", f.Name)
			}
		}
	}
}

func TestALibraryChartIsPublished(t *testing.T) {
	ff := extract(t, valuesRepo())
	for _, f := range ff {
		if f.Kind == facts.KindModule && f.Name == "lib" {
			if f.Props[facts.PropPublished] != true {
				t.Errorf("library module props = %v", f.Props)
			}
			return
		}
	}
	t.Error("lib module missing")
}

func TestReadsIn(t *testing.T) {
	cases := map[string][][]string{
		`{{ .Values.a.b }}`:                     {{"a", "b"}},
		`{{ $.Values.a }}`:                      {{"a"}},
		`{{ toYaml .Values | nindent 2 }}`:      {{}},
		`{{ index .Values "a" "b-c" }}`:         {{"a", "b-c"}},
		`{{ $root.Values.a }}`:                  {{"a"}},
		`{{ .context.Values.a.b }}`:             {{"a", "b"}},
		`{{ index $.Values.hooks $name }}`:      {{"hooks"}},
		`{{ index .Values.a "b" }}`:             {{"a", "b"}},
		`{{- with.Values.a }}`:                  {{"a"}},
		`{{ .Values.notValues }}`:               {{"notValues"}},
		`text .Values.a outside an action`:      nil,
		`{{ .Values.a }} and {{ .Values.b.c }}`: {{"a"}, {"b", "c"}},
	}
	for in, want := range cases {
		if got := readsIn(in); !reflect.DeepEqual(got, want) {
			t.Errorf("readsIn(%q) = %v, want %v", in, got, want)
		}
	}
	if got := pathStringsIn(`{{ include "x" (dict "providedValues" (list "server.masterKey" "plain")) }} "not.in.action"`); !reflect.DeepEqual(got, [][]string{{"server", "masterKey"}}) {
		t.Errorf("pathStringsIn = %v", got)
	}
}

// A value read only by a template written inside values.yaml, or named as a string
// for a helper to look up, is read.
func TestValuesReadFromValuesAndByName(t *testing.T) {
	files := valuesRepo()
	files["app/values.yaml"] += `memory:
  limit: 1Gi
configuration: |-
  vm_memory = {{ .Values.memory.limit }}
secretKey: ""
`
	files["app/templates/secret.yaml"] = `apiVersion: v1
kind: Secret
metadata:
  name: app
data:
  key: {{ include "lib.fullname" (dict "providedValues" (list "secretKey" "secretKey.missing") "context" $) }}
  {{- $cfg := tpl .Values.configuration . }}
`
	ff := extract(t, files)
	var vref *facts.Fact
	for i := range ff {
		if ff[i].Kind == facts.KindFileRef && ff[i].Name == "app/values.yaml" {
			vref = &ff[i]
		}
	}
	if got := targets(vref, facts.RelNames); !reflect.DeepEqual(got, []string{"app.values.memory", "app.values.memory.limit"}) {
		t.Errorf("values.yaml file_ref names = %v", got)
	}
	// "secretKey.missing" is not declared to its depth; "secretKey" alone is one
	// segment, not a dotted path, so neither string reads anything.
	sec := find(ff, "app.Secret.app")
	for _, n := range targets(sec, facts.RelNames) {
		if n == "app.values.secretKey" {
			t.Errorf("an undeclared dotted string resolved to its prefix: %v", targets(sec, facts.RelNames))
		}
	}
}

func TestActionLinesFollowAnActionAcrossLines(t *testing.T) {
	got := actionLines("a: {{ dict\n  \"k\" (.Values.x | toString)\n  \"j\" 1 }} b\nplain .Values.y\n{{ .Values.z }}")
	want := []string{"{{ dict}}", `{{  "k" (.Values.x | toString)}}`, `{{  "j" 1 }}`, "", "{{ .Values.z }}"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("actionLines = %q, want %q", got, want)
	}
}
