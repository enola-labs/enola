package facts

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The tests in this file pin the JSON field names of the snapshot artifacts —
// facts.jsonl (Fact, Relation) and insights.json (Insight, Evidence). That is
// the wire format external consumers parse without running enola's query layer
// (cognee re-materializes the graph in its own database from exactly these
// files), and until it was pinned nothing stopped a struct-tag rename from
// silently dropping fields in every downstream graph: the consumer probes for
// plausible key spellings and skips what it cannot find, so a rename degrades
// to missing edges rather than an error.
//
// contract_test.go guards the VOCABULARY that crosses the extractor->linker
// boundary (prop values, route sources); these guard the SHAPE that crosses the
// process boundary. Both are pure marshals, so they run in milliseconds.

// jsonKeys returns the top-level JSON field names v marshals to.
func jsonKeys(t *testing.T, v any) map[string]bool {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshaling %T: %v", v, err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshaling %T: %v", v, err)
	}
	keys := make(map[string]bool, len(m))
	for k := range m {
		keys[k] = true
	}
	return keys
}

func assertKeys(t *testing.T, v any, want []string) {
	t.Helper()
	wantSet := make(map[string]bool, len(want))
	for _, k := range want {
		wantSet[k] = true
	}
	if got := jsonKeys(t, v); !reflect.DeepEqual(got, wantSet) {
		t.Errorf("%T marshals to keys %v, want exactly %v", v, sortedKeys(got), want)
	}
}

// TestWireFormat_FactFields pins the Fact field names, both directions: a fully
// populated fact must expose exactly the documented keys (a rename or an added
// field fails), and a minimal fact must expose exactly kind+name (an omitempty
// dropped or gained fails). A consumer keys nodes on (repo, kind, name) and
// reads every other field as optional.
func TestWireFormat_FactFields(t *testing.T) {
	full := Fact{
		Kind:      KindModule,
		Name:      "cmd/enola",
		File:      "cmd/enola",
		Line:      1,
		EndLine:   2,
		Column:    3,
		EndColumn: 4,
		Repo:      "enola",
		Props:     map[string]any{"language": "go"},
		Relations: []Relation{{Kind: RelImports, Target: "context"}},
	}
	assertKeys(t, full, []string{
		"kind", "name", "file", "line", "end_line", "column", "end_column",
		"repo", "props", "relations",
	})

	minimal := Fact{Kind: KindModule, Name: "cmd/enola"}
	assertKeys(t, minimal, []string{"kind", "name"})
}

// TestWireFormat_RelationFields pins the Relation field names. A relation is a
// directed edge named by its target fact's NAME — the identity convention the
// whole format rests on, so both keys are load-bearing.
func TestWireFormat_RelationFields(t *testing.T) {
	assertKeys(t, Relation{Kind: RelCalls, Target: "foo"}, []string{"kind", "target"})
}

// TestWireFormat_InsightFields pins the Insight field names. Evidence has no
// omitempty: a finding with no evidence marshals an empty array, and a consumer
// iterating it must never see null.
func TestWireFormat_InsightFields(t *testing.T) {
	full := Insight{
		Title:       "Dependency cycle",
		Source:      "cycles",
		Description: "a -> b -> a",
		Confidence:  1.0,
		Evidence: []Evidence{{
			File: "a.go", Symbol: "A", Fact: "mod/a", Detail: "imports b",
			Line: 1, EndLine: 2, Column: 3, EndColumn: 4,
		}},
		Actions:       []string{"break the cycle"},
		Informational: true,
		Metrics:       map[string]any{"modules": 2},
	}
	assertKeys(t, full, []string{
		"title", "source", "description", "confidence", "evidence",
		"suggested_actions", "informational", "metrics",
	})

	minimal := Insight{Title: "t", Description: "d", Confidence: 1.0, Evidence: []Evidence{}}
	assertKeys(t, minimal, []string{"title", "description", "confidence", "evidence"})
}

// TestWireFormat_EvidenceFields pins the Evidence field names. An entry cites at
// most one of symbol/fact plus an optional file; the span fields carry the
// position the extractor measured, never a derivation from a name.
func TestWireFormat_EvidenceFields(t *testing.T) {
	assertKeys(t, Evidence{
		File: "a.go", Symbol: "A", Fact: "mod/a", Detail: "imports b",
		Line: 1, EndLine: 2, Column: 3, EndColumn: 4,
	}, []string{"file", "symbol", "fact", "detail", "line", "end_line", "column", "end_column"})
}

// TestWireFormat_ReceiptFields pins the receipt's field names. receipt.json is
// a contract too — it is where a consumer reads format_version to decide whether
// it can read the other two artifacts at all — and until this test existed the
// field that answers that question could be renamed with a green build.
//
// Only the objects docs/schema/receipt.md describes field-by-field are pinned
// exactly. ProviderRecord is deliberately absent: its diagnostic counters (cache
// reuse, per-provider census, extractor agreement) are reporting detail the doc
// declares out of contract, so pinning them would fail the build on a change no
// consumer can see.
func TestWireFormat_ReceiptFields(t *testing.T) {
	full := Receipt{
		SnapshotID:       "sha256:abc",
		FormatVersion:    ArtifactFormatVersion,
		EnolaVersion:     "dev",
		ExtractorVersion: "v1",
		GeneratedAt:      "2026-01-01T00:00:00Z",
		Duration:         "1s",
		RepoPath:         "/repo",
		Git:              &GitInfo{Ref: "main"},
		Extractors:       []string{"go"},
		Explainers:       []string{"cycles"},
		Renderers:        []string{"llm"},
		Providers:        []ProviderRecord{{Name: "p"}},
		ConfigHash:       "sha256:cfg",
		IgnoreGlobHash:   "sha256:glob",
		OutputHashes:     map[string]string{"facts.jsonl": "sha256:f"},
		FactCount:        1,
		InsightCount:     0,
	}
	assertKeys(t, full, []string{
		"snapshot_id", "format_version", "enola_version", "extractor_version",
		"generated_at", "duration", "repo_path", "git", "extractors", "explainers",
		"renderers", "providers", "config_hash", "ignore_glob_hash", "output_hashes",
		"fact_count", "insight_count", "quality",
	})

	// The always-written subset: a consumer may rely on these being present even
	// on a snapshot of an empty non-git directory with no providers configured.
	minimal := Receipt{}
	assertKeys(t, minimal, []string{
		"snapshot_id", "format_version", "enola_version", "generated_at",
		"duration", "repo_path", "extractors", "explainers", "fact_count",
		"insight_count", "quality",
	})
}

// TestWireFormat_ReceiptQualityFields pins the quality block — the loop signal a
// consumer polls to tell a thin extraction from a complete one. Its counters
// have no omitempty on purpose: a zero is a measured answer ("could not see:
// nothing"), not an absent one, and dropping the key would make the two
// indistinguishable.
func TestWireFormat_ReceiptQualityFields(t *testing.T) {
	full := ReceiptQuality{
		FilesSeen:         2,
		FilesParsed:       1,
		FilesSkipped:      1,
		DirsSkipped:       0,
		SkippedSample:     []string{"vendor/x.go (vendor/**)"},
		ParseErrors:       1,
		ParseErrorSample:  []ParseError{{Extractor: "go", File: "a.go", Msg: "boom"}},
		HeuristicInsights: 1,
		Coverage:          &CoverageSummary{ServicesTotal: 2},
		Census:            &FileCensus{FilesWalked: 2},
	}
	assertKeys(t, full, []string{
		"files_seen", "files_parsed", "files_skipped", "dirs_skipped",
		"skipped_sample", "parse_errors", "parse_error_sample",
		"heuristic_insights", "coverage", "census",
	})

	assertKeys(t, ReceiptQuality{}, []string{
		"files_seen", "files_parsed", "files_skipped", "dirs_skipped",
		"parse_errors", "heuristic_insights",
	})
}

// TestWireFormat_ReceiptNestedFields pins the objects receipt.md documents
// inside the receipt. They are small and every field of each is documented, so
// they are pinned exactly like Fact and Relation.
func TestWireFormat_ReceiptNestedFields(t *testing.T) {
	assertKeys(t, GitInfo{Ref: "main", Commit: "abc", Dirty: true, Remote: "github.com/o/r"},
		[]string{"ref", "commit", "dirty", "remote"})
	// Dirty has no omitempty: a clean tree must say so rather than say nothing.
	assertKeys(t, GitInfo{}, []string{"dirty"})

	assertKeys(t, ParseError{Extractor: "go", File: "a.go", Msg: "boom"},
		[]string{"extractor", "file", "msg"})

	assertKeys(t, CoverageSummary{
		ServicesTotal: 1, CoverageGaps: 1, UnresolvedEdges: 1, ExternalEdges: 1,
		ExtractorsReporting: 1, ExtractionUnresolved: 1,
	}, []string{
		"services_total", "coverage_gaps", "unresolved_edges", "external_edges",
		"extractors_reporting", "extraction_unresolved",
	})

	assertKeys(t, FileCensus{
		FilesWalked: 1, Parsed: 1, ExcludedByIgnore: 1, ExcludedByKind: 1,
		ExcludedKinds: map[string]int{"md": 1}, SkippedWithCause: 1,
		TopSkipCauses: []CensusCause{{Cause: "c", Count: 1}},
	}, []string{
		"files_walked", "parsed", "excluded_by_ignore", "excluded_by_kind",
		"excluded_kinds", "skipped_with_cause", "top_skip_causes",
	})

	assertKeys(t, CensusCause{Cause: "c", Count: 1}, []string{"cause", "count"})
}

// TestWireFormat_ArtifactFormatVersion holds the format generation to a value a
// human typed. Bumping it is a coordinated event — consumers pin an enola
// release by checksum and must re-pin to adopt a new generation — so it may not
// ride along with an edit to something else.
func TestWireFormat_ArtifactFormatVersion(t *testing.T) {
	if ArtifactFormatVersion != 1 {
		t.Errorf("ArtifactFormatVersion = %d, want 1; a bump ships in a major "+
			"release with a CHANGELOG migration note (docs/schema/README.md, "+
			"Versioning) — update this test as part of that release, not before",
			ArtifactFormatVersion)
	}
	if got := (SnapshotMeta{}).Receipt().FormatVersion; got != ArtifactFormatVersion {
		t.Errorf("Receipt().FormatVersion = %d, want %d: every receipt written "+
			"must be stamped, or a consumer reads zero and treats a current "+
			"snapshot as an unknown format", got, ArtifactFormatVersion)
	}
}

// vocabularyPrefixes names the constant families that are WIRE VALUES: a string
// a consumer reads out of facts.jsonl and branches on. They are exactly the
// vocabularies docs/schema/facts.md enumerates.
//
// Prop KEYS (the Prop* constants) are deliberately absent. They are documented
// per kind rather than as one list, and their names do not carry the value.
var vocabularyPrefixes = []string{
	"Kind", "Rel", "Via", "RouteSource", "RouteType", "Role", "Ecosystem",
	"Symbol", "ModuleRole", "Coupling", "DepSource", "Messaging", "Framework",
	"Type", "StorageKind", "MethodAny",
}

// TestWireFormat_DocumentedVocabulary fails when a registered wire value is
// missing from docs/schema/facts.md.
//
// The other tests here pin the SHAPE of the artifacts; this one pins the
// VOCABULARY's documentation. Both matter for the same reason: docs/schema/ is
// the contract an external consumer reads instead of this source tree, so a kind
// or source value that lands undocumented is a value nobody outside can map —
// and the failure mode is silent, a consumer skipping facts it does not
// recognize with a warning nobody reads.
//
// The check is presence of the value as inline code (`value`) rather than a
// prose match, so it costs one backticked mention and cannot be satisfied by the
// word occurring incidentally.
func TestWireFormat_DocumentedVocabulary(t *testing.T) {
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "schema", "facts.md"))
	if err != nil {
		t.Fatalf("reading the schema doc this package is the contract for: %v", err)
	}
	text := string(doc)

	values := vocabularyConstants(t)
	if len(values) < 50 {
		t.Fatalf("found only %d vocabulary constants — the scan is broken, not the "+
			"vocabulary; a passing run here would mean nothing", len(values))
	}

	var missing []string
	for value, names := range values {
		if !strings.Contains(text, "`"+value+"`") {
			missing = append(missing, value+" ("+strings.Join(names, ", ")+")")
		}
	}
	sort.Strings(missing)
	for _, m := range missing {
		t.Errorf("wire value %s is registered in code but absent from "+
			"docs/schema/facts.md: document it there (as `value`), or, if it is "+
			"not a value a consumer ever reads, name it outside the vocabulary "+
			"prefixes %v", m, vocabularyPrefixes)
	}
}

// vocabularyConstants returns every string constant in this package whose name
// starts with a vocabulary prefix, mapped to the constant names that spell it.
// Several names may share a value (RouteTypeGRPC and FrameworkGRPC are both
// "grpc"), and the reader wants to see all of them in a failure.
func vocabularyConstants(t *testing.T) map[string][]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading this package's directory: %v", err)
	}

	fset := token.NewFileSet()
	values := map[string][]string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, constName := range vs.Names {
					if i >= len(vs.Values) || !hasVocabularyPrefix(constName.Name) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue // iota, a computed value: not a wire string
					}
					v, err := strconv.Unquote(lit.Value)
					if err != nil || v == "" {
						continue
					}
					values[v] = append(values[v], constName.Name)
				}
			}
		}
	}
	for v := range values {
		sort.Strings(values[v])
	}
	return values
}

func hasVocabularyPrefix(name string) bool {
	for _, p := range vocabularyPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// TestWireFormat_WireFactFields pins the shape facts.jsonl actually carries.
//
// TestWireFormat_FactFields above pins the in-memory Fact; this pins the wire
// shape built from it, which is that plus the two ids. The pair is the point:
// they must differ by exactly `id` and `target_id` and nothing else, so a field
// added to Fact reaches consumers (both tests change) while a field added only
// here is caught as an undeclared divergence (one test changes).
func TestWireFormat_WireFactFields(t *testing.T) {
	full := wireFact{
		Fact: Fact{
			Kind: KindModule, Name: "cmd/enola", File: "cmd/enola", Line: 1,
			EndLine: 2, Column: 3, EndColumn: 4, Repo: "enola",
			Props: map[string]any{"language": "go"},
		},
		Relations: []wireRelation{{Relation: Relation{Kind: RelImports, Target: "context"}}},
		ID:        "0123456789abcdef0123456789abcdef",
	}
	assertKeys(t, full, []string{
		"kind", "name", "file", "line", "end_line", "column", "end_column",
		"repo", "props", "relations", "id",
	})

	// id has no omitempty: every fact carries one, so a consumer may key on it
	// unconditionally. A minimal fact is the struct's keys plus exactly that.
	assertKeys(t, wireFact{Fact: Fact{Kind: KindModule, Name: "cmd/enola"}},
		[]string{"kind", "name", "id"})
}

// TestWireFormat_WireRelationFields pins the edge shape. target_id IS omitempty:
// most targets name something outside the snapshot, and an empty string would
// make "nothing to point at" indistinguishable from "here is the answer".
func TestWireFormat_WireRelationFields(t *testing.T) {
	assertKeys(t, wireRelation{
		Relation: Relation{Kind: RelCalls, Target: "foo"},
		TargetID: "0123456789abcdef0123456789abcdef",
	}, []string{"kind", "target", "target_id"})

	assertKeys(t, wireRelation{Relation: Relation{Kind: RelCalls, Target: "foo"}},
		[]string{"kind", "target"})
}

// TestWireFormat_WireInsightFields pins the insights.json shape, the same way
// TestWireFormat_WireFactFields pins facts.jsonl: the model's keys plus exactly
// the one id field.
func TestWireFormat_WireInsightFields(t *testing.T) {
	full := wireInsight{
		Title: "Dependency cycle", Source: "cycles", Description: "a -> b -> a",
		Confidence: 1.0, Evidence: []wireEvidence{{}}, Actions: []string{"break it"},
		Informational: true, Metrics: map[string]any{"modules": 2},
	}
	assertKeys(t, full, []string{
		"title", "source", "description", "confidence", "evidence",
		"suggested_actions", "informational", "metrics",
	})

	assertKeys(t, wireInsight{Title: "t", Description: "d", Confidence: 1.0, Evidence: []wireEvidence{}},
		[]string{"title", "description", "confidence", "evidence"})
}

// TestWireFormat_WireEvidenceFields pins the evidence shape. fact_id is
// omitempty: a citation that resolves to nothing is a normal and sometimes
// correct outcome — a finding about a handler that is not defined here cites a
// name no fact carries, and that absence IS the finding.
func TestWireFormat_WireEvidenceFields(t *testing.T) {
	assertKeys(t, wireEvidence{
		Evidence: Evidence{File: "a.go", Symbol: "A", Fact: "mod/a", Detail: "d",
			Line: 1, EndLine: 2, Column: 3, EndColumn: 4},
		FactID: "0123456789abcdef0123456789abcdef",
	}, []string{"file", "symbol", "fact", "detail", "line", "end_line", "column", "end_column", "fact_id"})

	assertKeys(t, wireEvidence{Evidence: Evidence{File: "a.go"}}, []string{"file"})
}

// TestWireFormat_InsightKeyOrderIsUnchanged is why wireInsight restates its
// fields instead of embedding Insight.
//
// encoding/json emits an embedded struct's promoted fields before the outer
// struct's own, so shadowing `evidence` would silently move it from the middle
// of every insight object to the end — rewriting every line of every insights
// golden and burying the one changed finding a reviewer is trying to see. This
// fails if anyone converts wireInsight to the embedded form.
func TestWireFormat_InsightKeyOrderIsUnchanged(t *testing.T) {
	model := Insight{
		Title: "t", Source: "s", Description: "d", Confidence: 1.0,
		Evidence: []Evidence{{}}, Actions: []string{"a"},
		Informational: true, Metrics: map[string]any{"n": 1},
	}
	wire := wireInsight{
		Title: "t", Source: "s", Description: "d", Confidence: 1.0,
		Evidence: []wireEvidence{{}}, Actions: []string{"a"},
		Informational: true, Metrics: map[string]any{"n": 1},
	}
	got, want := keyOrder(t, wire), keyOrder(t, model)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("insights.json key order changed\n got %v\nwant %v", got, want)
	}
}

// keyOrder returns v's top-level JSON keys in the order they are emitted.
func keyOrder(t *testing.T, v any) []string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshaling %T: %v", v, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if _, err := dec.Token(); err != nil { // opening brace
		t.Fatalf("reading %T: %v", v, err)
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("reading %T: %v", v, err)
		}
		keys = append(keys, tok.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			t.Fatalf("reading %T: %v", v, err)
		}
	}
	return keys
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for key := range set {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
