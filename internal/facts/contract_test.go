package facts

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The two tests here are the enforcement behind contract.go. Constants alone do not stop
// drift — nothing prevents an extractor from writing the literal anyway, which is exactly
// how the linker came to miss the two Java client sources. These make the two failure
// modes fail the build:
//
//   - TestContract_GoldenRouteSourcesAreRegistered catches a route source that EXISTS in
//     extracted output but is unknown to this file, so the linker cannot classify it.
//   - TestContract_NoHandWrittenContractLiterals catches a contract value spelled as a
//     literal at a call site, which is what lets the two sides drift in the first place.
//
// Both are pure file scans with no engine or tree-sitter dependency, so they run in
// milliseconds — cheap enough for a pre-push hook, like internal/cachecov.

// repoRoot walks up from this package to the module root, so the scans below do not
// depend on the test's working directory beyond its own package.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for range 10 {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("could not locate module root (no go.mod found walking up)")
	return ""
}

// TestContract_GoldenRouteSourcesAreRegistered asserts every `source` value carried by a
// route fact in the golden corpus is a registered RouteSource.
//
// The golden corpus is the only place the project records what extractors ACTUALLY emit,
// so it is the honest input for this check: a new extractor pass that invents a source
// value lands in a golden before it lands anywhere else. Scoping to KindRoute matters —
// the same prop key carries the unrelated DepSource vocabulary on dependency facts.
func TestContract_GoldenRouteSourcesAreRegistered(t *testing.T) {
	goldenDir := filepath.Join(repoRoot(t), "internal", "engine", "testdata", "golden")
	entries, err := os.ReadDir(goldenDir)
	if err != nil {
		t.Fatalf("reading golden dir: %v", err)
	}

	seen := map[string]string{} // source value -> first golden file it appeared in
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(goldenDir, e.Name())
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("opening %s: %v", e.Name(), err)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 1<<20), 16<<20) // golden lines can be long
		for sc.Scan() {
			var fact struct {
				Kind  string         `json:"kind"`
				Props map[string]any `json:"props"`
			}
			if err := json.Unmarshal(sc.Bytes(), &fact); err != nil {
				continue // not a fact line
			}
			if fact.Kind != KindRoute {
				continue
			}
			src, _ := fact.Props[PropSource].(string)
			if src == "" {
				continue
			}
			if _, ok := seen[src]; !ok {
				seen[src] = e.Name()
			}
		}
		_ = f.Close()
	}

	if len(seen) == 0 {
		t.Fatal("no route source values found in the golden corpus — the scan is not reading anything")
	}
	for src, file := range seen {
		if !AllRouteSources[src] {
			t.Errorf("route source %q (in testdata/golden/%s) is not registered in AllRouteSources.\n"+
				"Add a RouteSource constant for it in contract.go, and decide whether it belongs in "+
				"HandWrittenClientSources — a hand-written call site links as via=\"http-client\", a "+
				"contract-derived route as via=\"http\".", src, file)
		}
	}
}

// contractLiterals are the values that MUST be written as constants rather than spelled
// out, mapped to the constant to use. Every one is read by a package other than the one
// that writes it, so a literal here is an untracked cross-package coupling.
//
// DepSource values are deliberately absent: nothing outside the extractor that writes
// them branches on them, so they are not a contract and stay literals.
func contractLiterals() map[string]map[string]string {
	handWritten := map[string]string{}
	for src := range AllRouteSources {
		handWritten[src] = "facts.RouteSource… (see contract.go)"
	}
	return map[string]map[string]string{
		PropSource:    handWritten,
		PropRole:      {RoleClient: "facts.RoleClient", RoleServer: "facts.RoleServer"},
		PropFramework: {FrameworkGRPC: "facts.FrameworkGRPC"},
	}
}

// propAssign matches a prop written as a string-literal key with a string-literal value,
// in either of the two forms the codebase uses: a map entry (`"role": "client",`) and an
// index assignment (`f.Props["role"] = "client"`).
var propAssign = regexp.MustCompile(`"(source|role|framework)"\s*(?::|\]\s*=)\s*"([a-zA-Z0-9/_-]+)"`)

// TestContract_NoHandWrittenContractLiterals asserts no production file spells a contract
// value out instead of using its constant.
//
// This is the check that actually prevents drift. The golden test above only sees values
// that reached a fixture; this one fails the moment the code is written, and it applies to
// readers as much as writers — a linker comparing against a literal is the same bug from
// the other side.
func TestContract_NoHandWrittenContractLiterals(t *testing.T) {
	root := repoRoot(t)
	want := contractLiterals()

	// Scanned: the packages that WRITE contract values (extractors) and those that READ
	// them (linkers, engine binders). facts/ itself is excluded — this file and
	// contract.go are where the literals are legitimately defined.
	scanDirs := []string{
		filepath.Join(root, "internal", "extractors"),
		filepath.Join(root, "internal", "linkers"),
		filepath.Join(root, "internal", "engine"),
	}

	for _, dir := range scanDirs {
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			for i, line := range strings.Split(string(src), "\n") {
				// Prose mentions a value freely — the doc comments explaining WHY a
				// binder keys on "python-grpc-client" are the record of that decision
				// and must stay readable.
				if strings.HasPrefix(strings.TrimSpace(line), "//") {
					continue
				}
				for _, m := range propAssign.FindAllStringSubmatch(line, -1) {
					key, val := m[1], m[2]
					if replacement, ok := want[key][val]; ok {
						t.Errorf("%s:%d: %q is a contract value written as a literal — use %s\n\t%s",
							rel, i+1, val, replacement, strings.TrimSpace(line))
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", dir, err)
		}
	}
}

func TestViaKinds_RegistryComplete(t *testing.T) {
	for _, v := range []string{ViaHTTP, ViaHTTPClient, ViaGRPC, ViaGraphQL, ViaKafka, ViaImport, ViaSharedSymbols, ViaObjectStorage, ViaDeployment} {
		if !AllViaKinds[v] {
			t.Errorf("via constant %q missing from AllViaKinds — intent validation would reject a value a signal emits", v)
		}
	}
	if len(AllViaKinds) != 9 {
		t.Errorf("AllViaKinds has %d entries; a new via kind must be added to both the constants and this test", len(AllViaKinds))
	}
}

func TestMessagingProtocolFamily(t *testing.T) {
	tests := map[string]string{
		"kafka": "kafka", "KAFKA": "kafka", " kafka-secure ": "kafka",
		"mqtt": "mqtt", " WSS ": "wss", "": "",
	}
	for protocol, want := range tests {
		if got := MessagingProtocolFamily(protocol); got != want {
			t.Errorf("MessagingProtocolFamily(%q) = %q, want %q", protocol, got, want)
		}
	}
}

func TestIsKafkaProtocol(t *testing.T) {
	for protocol, want := range map[string]bool{
		"kafka": true, "KAFKA": true, " kafka-secure ": true,
		"mqtt": false, "kafkaish": false, "": false,
	} {
		if got := IsKafkaProtocol(protocol); got != want {
			t.Errorf("IsKafkaProtocol(%q) = %v, want %v", protocol, got, want)
		}
	}
}
