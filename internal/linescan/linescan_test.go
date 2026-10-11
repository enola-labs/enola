package linescan

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
)

func lines(t *testing.T, input string) []string {
	t.Helper()
	sc := New(strings.NewReader(input))
	var out []string
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("Err = %v", err)
	}
	return out
}

func TestLineEndings(t *testing.T) {
	cases := map[string][]string{
		"":             nil,
		"a":            {"a"},
		"a\n":          {"a"},
		"a\nb":         {"a", "b"},
		"a\r\nb\r\n":   {"a", "b"},
		"a\n\nb\n":     {"a", "", "b"},
		"\n":           {""},
		"a b\nc":       {"a b", "c"},
		"a\rb\n":       {"a\rb"},
		"last\r":       {"last"},
		"x\n\n":        {"x", ""},
		"tab\tkept \n": {"tab\tkept "},
	}
	for input, want := range cases {
		got := lines(t, input)
		if strings.Join(got, "|") != strings.Join(want, "|") || len(got) != len(want) {
			t.Errorf("%q: got %q, want %q", input, got, want)
		}
	}
}

// A line longer than any bufio.Scanner limit in use (the largest was 16 MB) is read
// whole, and so is what follows it.
func TestLongLineDoesNotEndTheScan(t *testing.T) {
	long := strings.Repeat("x", 20*1024*1024)
	got := lines(t, "before\n"+long+"\nafter\n")
	if len(got) != 3 || got[0] != "before" || len(got[1]) != len(long) || got[2] != "after" {
		t.Fatalf("got %d lines, lengths %d/%d; want before, the long line, after", len(got), len(got[0]), len(got[len(got)-1]))
	}
}

func TestReadErrorIsReported(t *testing.T) {
	boom := errors.New("boom")
	sc := New(io.MultiReader(strings.NewReader("a\nb"), iotest.ErrReader(boom)))
	var got []string
	for sc.Scan() {
		got = append(got, sc.Text())
	}
	if len(got) != 1 || got[0] != "a" {
		t.Errorf("lines before the error = %q, want [a]: a line cut short by the error is not a line", got)
	}
	if !errors.Is(sc.Err(), boom) {
		t.Errorf("Err = %v, want %v", sc.Err(), boom)
	}
	if sc.Scan() {
		t.Error("Scan after an error returned true")
	}
}

// The extractors read source files, and a source file may hold a line of any length.
// bufio.Scanner would stop at its limit, so none of them may use it.
func TestExtractorsDoNotUseBufioScanner(t *testing.T) {
	root := filepath.Join("..", "extractors")
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(src), "bufio.NewScanner(") {
			t.Errorf("%s uses bufio.NewScanner, which stops at a line over its limit; use linescan.New", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
