package pythonextractor

import (
	"context"
	"slices"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

func TestExtractConfiguredEntryPoints(t *testing.T) {
	repo := t.TempDir()
	files := []string{"pkg/service.py", "image/app.py", "config.yaml", "settings.ini", "image/Dockerfile"}
	writePy(t, repo, files[0], "def factory(): pass\ndef configured(): pass\ndef unused(): pass\n")
	writePy(t, repo, files[1], "def lambda_handler(event, context): pass\ndef unused(): pass\n")
	writePy(t, repo, files[2], `factory: pkg.service:factory
description: Use pkg.service.unused when needed
"pkg.service.unused": ignored
external: thirdparty.service.unused
missing: pkg.service.missing
`)
	writePy(t, repo, files[3], "[runtime]\nhostname_callable = pkg.service.configured\n# ignored = pkg.service.unused\n")
	writePy(t, repo, files[4], "FROM public.ecr.aws/lambda/python:3.12\nCMD [\"app.lambda_handler\"]\n")
	ff, err := New().Extract(context.Background(), repo, files)
	if err != nil {
		t.Fatal(err)
	}
	refs := make(map[string]map[string]bool)
	for _, f := range ff {
		if f.Kind == facts.KindFileRef && f.PropAny("reference_source") == "configuration" {
			refs[f.File] = targets(f)
		}
	}
	for file, target := range map[string]string{
		"config.yaml":      "pkg/service.factory",
		"settings.ini":     "pkg/service.configured",
		"image/Dockerfile": "image/app.lambda_handler",
	} {
		if len(refs[file]) != 1 || !refs[file][target] {
			t.Errorf("%s references %v, want only %s", file, refs[file], target)
		}
	}
}

func TestConfiguredValuesDoNotTreatDockerArgumentsAsHandlers(t *testing.T) {
	for _, content := range []string{
		`CMD ["echo", "app.unused"]`,
		`RUN echo "app.unused"`,
		`# CMD ["app.unused"]`,
	} {
		if refs := configuredPythonValues("Dockerfile", []byte(content)); len(refs) != 0 {
			t.Errorf("%q yielded %v", content, refs)
		}
	}
}

func TestAST_FactoryStringsAndFallbackCallbacks(t *testing.T) {
	src := `
def default_processor(): pass
def initialize(processor=None):
    selected = processor or default_processor
    serve("pkg.service:factory")
    serve(f"pkg.service:unused{processor}")
`
	ff := astExtractIdx(t, "service.py", src)
	f := byName(ff)["service.initialize"]
	if !slices.Contains(relsByKind(f, facts.RelNames), "service.default_processor") || hasCallTo(f, "service.default_processor") {
		t.Errorf("fallback must name its callback without inventing a call: %+v", f.Relations)
	}
	if !hasCallTo(f, "pkg.service:factory") {
		t.Error("missing configured factory reference")
	}
	if hasCallTo(f, "pkg.service.unused") {
		t.Error("interpolated strings must not be treated as literal entry points")
	}
	shadowed := astExtractIdx(t, "service.py", `
def default_processor(): pass
def initialize(default_processor, processor=None):
    selected = processor or default_processor
`)
	if slices.Contains(relsByKind(byName(shadowed)["service.initialize"], facts.RelNames), "service.default_processor") {
		t.Error("a shadowing parameter must not reference the module-level function")
	}
}

func TestFactoryStringsRequireDeclaredSymbols(t *testing.T) {
	repo := t.TempDir()
	files := []string{"docs/source/en/_config.py", "pkg/service.py"}
	writePy(t, repo, files[0], "title = 'English'\n")
	writePy(t, repo, files[1], `
def factory(): pass
def start():
    serve("pkg.service:factory")
    assert entity == "en:Japan"
    serve("pkg.service:missing")
`)
	ff, err := New().Extract(context.Background(), repo, files)
	if err != nil {
		t.Fatal(err)
	}
	f := byName(ff)["pkg/service.start"]
	if !hasCallTo(f, "pkg/service.factory") {
		t.Errorf("declared factory was lost: %+v", f.Relations)
	}
	for _, target := range []string{"en.Japan", "en:Japan", "pkg/service.missing", "pkg.service:missing"} {
		if hasCallTo(f, target) {
			t.Errorf("data or missing factory produced a call to %s", target)
		}
	}
}

func TestAST_LiteralReflectionAndCallableMetadata(t *testing.T) {
	ff := astExtractIdx(t, "service.py", `
from pkg import permissions
def hostname(): pass
def load():
    callback = getattr(permissions, "resource_name_for_dag")
    configured = f"{hostname.__module__}.hostname"
`)
	f := byName(ff)["service.load"]
	for _, target := range []string{"pkg.permissions.resource_name_for_dag", "service.hostname"} {
		if !slices.Contains(relsByKind(f, facts.RelNames), target) || hasCallTo(f, target) {
			t.Errorf("reflection/metadata must name %s without inventing a call: %+v", target, f.Relations)
		}
	}
	shadowed := astExtractIdx(t, "service.py", `
from pkg import permissions
def load(getattr):
    getattr(permissions, "resource_name_for_dag")
`)
	if slices.Contains(relsByKind(byName(shadowed)["service.load"], facts.RelNames), "pkg.permissions.resource_name_for_dag") {
		t.Error("shadowed getattr is not builtin reflective dispatch")
	}
}

func TestAST_FallbackConstantsAreNotConstructors(t *testing.T) {
	ff := astExtractIdx(t, "service.py", `
API_ROOT_PATH = "/api"
def path(root=None):
    return root or API_ROOT_PATH
`)
	f := byName(ff)["service.path"]
	if slices.Contains(relsByKind(f, facts.RelInstantiates), "API_ROOT_PATH") {
		t.Error("reading a constant must not invent a constructor edge")
	}
}

func TestPythonConfigurationAffectsCacheKey(t *testing.T) {
	for _, file := range []string{"runtime/config.yaml", "settings.ini", "image/Dockerfile", "pyproject.toml"} {
		if !New().AffectsKey(file) {
			t.Errorf("configuration %s must invalidate Python references", file)
		}
	}
	if New().AffectsKey("web/app.ts") || New().OwnsFile("config.yaml") {
		t.Error("key dependency must not claim configuration ownership or unrelated source")
	}
}
