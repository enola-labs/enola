package command

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/locate"
)

// A stored snapshot answers on its own: no working tree beside it, and no snapshot taken.
func TestLocate_FromStoredSnapshot(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".enola")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, f := range []facts.Fact{
		{Kind: facts.KindSymbol, Name: "src.alpha", File: "src/a.ts", Line: 3, EndLine: 9, Repo: "app",
			Props: map[string]any{"symbol_kind": "function", facts.PropBodyHash: "aaaa"}},
		{Kind: facts.KindSymbol, Name: "src.beta", File: "src/a.ts", Line: 11, EndLine: 20, Repo: "app",
			Props: map[string]any{"symbol_kind": "function", facts.PropBodyHash: "bbbb"}},
	} {
		b, err := json.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(b))
	}
	if err := os.WriteFile(filepath.Join(dir, "facts.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	alpha := facts.FactID("app", facts.KindSymbol, "src.alpha", "src/a.ts")
	stamps := filepath.Join(t.TempDir(), "stamps.jsonl")
	if err := os.WriteFile(stamps, []byte(`{"id":"`+alpha+`","body_hash":"old","name":"src.alpha"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, from := range []string{dir, filepath.Join(dir, "facts.jsonl")} {
		var out, errOut bytes.Buffer
		code := testRunner().runLocate(context.Background(),
			[]string{"--facts", from, "--json", "--stamps", stamps, "src/a.ts:5"}, nil, &out, &errOut)
		if code != 0 {
			t.Fatalf("--facts %s: exit %d: %s", from, code, errOut.String())
		}
		var got struct {
			Locations []locate.LocationResult `json:"locations"`
			Stamps    []locate.CheckResult    `json:"stamps"`
		}
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Locations) != 1 || len(got.Locations[0].Symbols) != 1 ||
			got.Locations[0].Symbols[0].ID != alpha || got.Locations[0].Symbols[0].BodyHash != "aaaa" {
			t.Errorf("--facts %s: locations %+v, want src.alpha with its id and body", from, got.Locations)
		}
		if len(got.Stamps) != 1 || got.Stamps[0].Status != locate.StatusRewritten {
			t.Errorf("--facts %s: stamps %+v, want rewritten", from, got.Stamps)
		}
	}

	for name, args := range map[string][]string{
		"with --at":       {"--facts", dir, "--at", "HEAD", "src/a.ts:5"},
		"with a repo":     {"--facts", dir, "src/a.ts:5", t.TempDir()},
		"not facts.jsonl": {"--facts", stamps, "src/a.ts:5"},
		"no snapshot":     {"--facts", t.TempDir(), "src/a.ts:5"},
	} {
		var out, errOut bytes.Buffer
		if code := testRunner().runLocate(context.Background(), args, nil, &out, &errOut); code != 2 {
			t.Errorf("%s: exit %d, want 2 (%s)", name, code, errOut.String())
		}
	}
}

// A decorator is part of a symbol's code in every language: changing one rewrites the
// symbol, and a citation on one lands on it, while line stays on the declaration.
func TestSpanStart_DecoratorsAcrossLanguages(t *testing.T) {
	for _, tc := range []struct {
		file, before, after string
		decorator, line     int
	}{
		{"a.py", "@cache\n@log(\n    1,\n)\ndef run(x):\n    return x\n", "@cache\n@log(\n    2,\n)\ndef run(x):\n    return x\n", 1, 5},
		{"b.ts", "class S {\n  @Log()\n  // why\n  run() {\n    return 1;\n  }\n}\n", "class S {\n  @Trace()\n  // why\n  run() {\n    return 1;\n  }\n}\n", 2, 4},
		{"c.ts", "@Injectable()\nexport class C {\n}\n", "@Other()\nexport class C {\n}\n", 1, 2},
		{"d.rs", "// licence\n#[inline]\npub fn run() -> i32 {\n    1\n}\n", "// licence\n#[cold]\npub fn run() -> i32 {\n    1\n}\n", 2, 3},
		{"e.dart", "class G {\n  @override\n  int run() {\n    return 1;\n  }\n}\n", "class G {\n  @deprecated\n  int run() {\n    return 1;\n  }\n}\n", 2, 3},
	} {
		t.Run(tc.file, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			repo := t.TempDir()
			path := filepath.Join(repo, tc.file)
			if err := os.WriteFile(path, []byte(tc.before), 0o644); err != nil {
				t.Fatal(err)
			}
			run := func(args ...string) (locations []locate.LocationResult, stamps []locate.CheckResult) {
				t.Helper()
				var out, errOut bytes.Buffer
				if code := testRunner().runLocate(context.Background(), append([]string{"--json"}, append(args, repo)...), nil, &out, &errOut); code != 0 {
					t.Fatalf("exit %d: %s", code, errOut.String())
				}
				var got struct {
					Locations []locate.LocationResult `json:"locations"`
					Stamps    []locate.CheckResult    `json:"stamps"`
				}
				if err := json.Unmarshal(out.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				return got.Locations, got.Stamps
			}

			loc, _ := run(tc.file + ":" + strconv.Itoa(tc.decorator))
			if len(loc) != 1 || len(loc[0].Symbols) != 1 {
				t.Fatalf("the decorator line located %+v, want its symbol", loc)
			}
			s := loc[0].Symbols[0]
			if s.Line != tc.line || s.SpanStart != tc.decorator {
				t.Errorf("line %d, span_start %d; want %d and %d", s.Line, s.SpanStart, tc.line, tc.decorator)
			}

			if err := os.WriteFile(path, []byte(tc.after), 0o644); err != nil {
				t.Fatal(err)
			}
			stampFile := filepath.Join(t.TempDir(), "stamps.jsonl")
			line, err := json.Marshal(locate.Stamp{ID: s.ID, BodyHash: s.BodyHash, Name: s.Name})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(stampFile, line, 0o644); err != nil {
				t.Fatal(err)
			}
			_, checked := run("--stamps", stampFile)
			if len(checked) != 1 || checked[0].Status != locate.StatusRewritten {
				t.Errorf("after the decorator changed: %+v, want rewritten", checked)
			}
		})
	}
}
