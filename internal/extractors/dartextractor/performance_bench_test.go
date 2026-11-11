package dartextractor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// Keep call classification and resolution linear as the number of distinct
// calls in a body grows. Restore the private metadata consumed by each pass.
func BenchmarkDartResolveCalls(b *testing.B) {
	for _, size := range []int{32, 256, 1024} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			owner := facts.Fact{Kind: facts.KindSymbol, Name: "lib.Owner", File: "lib/a.dart", Props: map[string]any{"symbol_kind": facts.SymbolClass}}
			caller := facts.Fact{Kind: facts.KindSymbol, Name: "lib.Owner.run", File: "lib/a.dart", Props: map[string]any{"symbol_kind": facts.SymbolMethod}}
			all := []facts.Fact{owner, caller}
			names := make([]string, size)
			for i := range size {
				name := fmt.Sprintf("lib.Owner.method%d", i)
				names[i] = name
				all[1].Relations = append(all[1].Relations, facts.Relation{Kind: facts.RelCalls, Target: name})
				all = append(all, facts.Fact{Kind: facts.KindSymbol, Name: name, File: "lib/a.dart", Props: map[string]any{"symbol_kind": facts.SymbolMethod}})
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				all[1].Props["_dart_implicit_calls"] = names
				all[1].Props["_dart_instance_calls"] = names
				resolveCallTargets(all)
			}
		})
	}
}

// Many methods share one class field environment. A method's cost should not
// include cloning all fields into each of its lexical scopes.
func BenchmarkDartFieldEnvironment(b *testing.B) {
	for _, size := range []int{16, 128, 512} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			var src strings.Builder
			src.WriteString("class Client { void get() {} } class Work {\n")
			for i := range size {
				fmt.Fprintf(&src, "Client client%d;\n", i)
			}
			for i := range 32 {
				fmt.Fprintf(&src, "void run%d() { client0.get(); }\n", i)
			}
			src.WriteString("}")
			data := []byte(src.String())
			pkgs := &packageIndex{byName: map[string]*pubPackage{}}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				extractFile(data, "lib/work.dart", pkgs, nil)
			}
		})
	}
}

// A file's function environment is immutable and shared by every class.
func BenchmarkDartFileEnvironment(b *testing.B) {
	var src strings.Builder
	for i := range 512 {
		fmt.Fprintf(&src, "void helper%d() {}\n", i)
	}
	for i := range 64 {
		fmt.Fprintf(&src, "class Work%d { void run() { helper0(); } }\n", i)
	}
	data := []byte(src.String())
	pkgs := &packageIndex{byName: map[string]*pubPackage{}}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		extractFile(data, "lib/work.dart", pkgs, nil)
	}
}

func dartPerformanceCorpus(tb testing.TB) (string, []string) {
	tb.Helper()
	repo := os.Getenv("ENOLA_DART_BENCH_REPO")
	if repo == "" {
		tb.Skip("set ENOLA_DART_BENCH_REPO to benchmark an existing Dart checkout")
	}
	var files []string
	err := filepath.WalkDir(repo, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != repo && (skipDetectDir(d.Name()) || d.Name() == ".enola") {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(repo, path)
		if err != nil {
			return err
		}
		if isDartFile(rel) && !isGeneratedDart(rel) {
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		tb.Fatal(err)
	}
	if len(files) == 0 {
		tb.Fatal("corpus has no Dart source")
	}
	return repo, files
}

func BenchmarkDartCorpus(b *testing.B) {
	repo, files := dartPerformanceCorpus(b)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := New().Extract(context.Background(), repo, files); err != nil {
			b.Fatal(err)
		}
	}
}

// Compare this digest between implementations using the same corpus revision
// and file list, independently of benchmark timing.
func TestDartPerformanceCorpusFacts(t *testing.T) {
	repo, files := dartPerformanceCorpus(t)
	all, err := New().Extract(context.Background(), repo, files)
	if err != nil {
		t.Fatal(err)
	}
	store := facts.NewStore()
	store.Add(all...)
	var buf bytes.Buffer
	if err := store.WriteJSONL(&buf); err != nil {
		t.Fatal(err)
	}
	if path := os.Getenv("ENOLA_DART_BENCH_FACTS"); path != "" {
		if err := os.WriteFile(path, buf.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("files=%d facts=%d sha256=%x", len(files), len(all), sha256.Sum256(buf.Bytes()))
}

var dartBenchmarkHost string

func BenchmarkDartLibraryLookup(b *testing.B) {
	for _, depth := range []int{0, 1, 16} {
		b.Run(fmt.Sprint(depth), func(b *testing.B) {
			all := []facts.Fact{}
			for i := 0; i <= depth; i++ {
				props := map[string]any{}
				if i < depth {
					props["library_file"] = fmt.Sprintf("lib/part%d.dart", i+1)
				}
				all = append(all, facts.Fact{Kind: facts.KindFileRef, File: fmt.Sprintf("lib/part%d.dart", i), Props: props})
			}
			resolver := newDartMemberResolver(all)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				dartBenchmarkHost = resolver.libraryFile("lib/part0.dart")
			}
		})
	}
}

func BenchmarkDartNestedScopes(b *testing.B) {
	for _, depth := range []int{4, 16, 64} {
		b.Run(fmt.Sprint(depth), func(b *testing.B) {
			var src strings.Builder
			src.WriteString("void callback() {} void run(")
			for i := range 32 {
				if i > 0 {
					src.WriteString(",")
				}
				fmt.Fprintf(&src, "dynamic arg%d", i)
			}
			src.WriteString(") {")
			for i := range depth {
				fmt.Fprintf(&src, "{ final local%d=arg0; consume(callback,local%d);", i, i)
			}
			src.WriteString(strings.Repeat("}", depth) + "}")
			data := []byte(src.String())
			pkgs := &packageIndex{byName: map[string]*pubPackage{}}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				extractFile(data, "lib/work.dart", pkgs, nil)
			}
		})
	}
}
