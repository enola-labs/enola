package engine

import (
	"testing"

	"github.com/enola-labs/enola/internal/clientspec"
	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/extractors"
)

func testSpec(name string) clientspec.Spec {
	path, service := 1, 0
	return clientspec.Spec{
		Name:          name,
		Language:      "typescript",
		ReceiverTypes: []string{"IHttpRequestService"},
		Methods:       []clientspec.Method{{Name: "sendRequest", PathArg: &path, ServiceArg: &service}},
	}
}

// TestConfigHash_FoldsClientSpecsAndAliases: specs and aliases change emitted facts,
// so two snapshots taken under different ones must not compare as equivalent. Neither
// may move the hash of a config that declares nothing.
func TestConfigHash_FoldsClientSpecsAndAliases(t *testing.T) {
	base := computeConfigHash(config.Default())

	empty := config.Default()
	empty.Clients = []clientspec.Spec{}
	empty.ServiceAliases = map[string]string{}
	if computeConfigHash(empty) != base {
		t.Error("declaring no specs and no aliases changed the config hash")
	}

	withSpec := config.Default()
	withSpec.Clients = []clientspec.Spec{testSpec("sdk-http")}
	if computeConfigHash(withSpec) == base {
		t.Error("a client spec did not change the config hash")
	}

	renamed := config.Default()
	renamed.Clients = []clientspec.Spec{testSpec("other")}
	if computeConfigHash(renamed) == computeConfigHash(withSpec) {
		t.Error("two different specs hash the same")
	}

	withAlias := config.Default()
	withAlias.ServiceAliases = map[string]string{"resource-api": "gateway"}
	if computeConfigHash(withAlias) == base {
		t.Error("a service alias did not change the config hash")
	}
}

// keyedExtractor is a fakeExtractor whose output also depends on configuration.
type keyedExtractor struct {
	fakeExtractor
	key string
}

func (k *keyedExtractor) ConfigKey() string { return k.key }

func keysWith(ts extractors.Extractor) map[string]string {
	hashes := map[string]string{"a.py": "h1", "b.ts": "h2", "go.mod": "h3"}
	files := []string{"a.py", "b.ts", "go.mod"}
	all := []extractors.Extractor{&fakeExtractor{name: "py", ext: ".py"}, ts}
	return computeExtractorKeys(all, files, hashes)
}

// TestComputeExtractorKeys_ConfigKey: editing configuration an extractor reads must
// bust its cache with no file changed, or it serves facts extracted under the old
// config. An empty key must key exactly as an extractor without the interface does,
// so nothing configured keeps every existing cache.
func TestComputeExtractorKeys_ConfigKey(t *testing.T) {
	plain := keysWith(&fakeExtractor{name: "ts", ext: ".ts"})
	emptyKey := keysWith(&keyedExtractor{fakeExtractor{name: "ts", ext: ".ts"}, ""})
	a := keysWith(&keyedExtractor{fakeExtractor{name: "ts", ext: ".ts"}, "spec-a"})
	b := keysWith(&keyedExtractor{fakeExtractor{name: "ts", ext: ".ts"}, "spec-b"})

	if emptyKey["ts"] != plain["ts"] {
		t.Error("an empty config key changed the cache key")
	}
	if a["ts"] == plain["ts"] {
		t.Error("a config key did not change the cache key")
	}
	if a["ts"] == b["ts"] {
		t.Error("different config keys produced the same cache key")
	}
	if a["py"] != plain["py"] {
		t.Error("one extractor's config key changed another extractor's cache key")
	}
}

// consumerExtractor records the specs the engine hands it.
type consumerExtractor struct {
	fakeExtractor
	got    []clientspec.Spec
	called bool
}

func (c *consumerExtractor) SetClientSpecs(specs []clientspec.Spec) {
	c.got, c.called = specs, true
}

// TestRegisterExtractor_HandsConsumersTheirSpecs: the engine is the one place specs
// reach an extractor, filtered to its language, so an extractor never reads config
// and a wrapper's extractor is configured the same way as a built-in one.
func TestRegisterExtractor_HandsConsumersTheirSpecs(t *testing.T) {
	cfg := config.Default()
	// Two specs on different types: the same type and method in both would be two
	// readings of one call, which validation rejects.
	b := testSpec("b")
	b.ReceiverTypes = []string{"LegacyHttpClient"}
	cfg.Clients = []clientspec.Spec{testSpec("a"), b}
	eng, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	ts := &consumerExtractor{fakeExtractor: fakeExtractor{name: "typescript", ext: ".ts"}}
	py := &consumerExtractor{fakeExtractor: fakeExtractor{name: "python", ext: ".py"}}
	eng.RegisterExtractor(ts)
	eng.RegisterExtractor(py)

	if !ts.called || len(ts.got) != 2 || ts.got[0].Name != "a" || ts.got[1].Name != "b" {
		t.Errorf("typescript consumer got %+v (called=%v), want specs a and b in order", ts.got, ts.called)
	}
	if !py.called || len(py.got) != 0 {
		t.Errorf("python consumer got %+v (called=%v), want an explicit empty set", py.got, py.called)
	}
}

// TestNew_RejectsInvalidClientSpecs: an engine built from a config assembled in code
// validates it exactly as a loaded file is validated.
func TestNew_RejectsInvalidClientSpecs(t *testing.T) {
	cfg := config.Default()
	bad := testSpec("a")
	bad.Methods[0].PathArg = nil
	cfg.Clients = []clientspec.Spec{bad}
	if _, err := New(cfg); err == nil {
		t.Fatal("engine accepted a spec with no path_arg")
	}
}
