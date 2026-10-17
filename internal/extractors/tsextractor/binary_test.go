package tsextractor

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestBinaryFilesWithATSExtensionAreSkipped(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, content []byte) {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	segment := append([]byte("export function sneaky() { return 2 }\n"), bytes.Repeat([]byte{0x47, 0x40, 0x11, 0x15, 0x00, 0x42, 0xf0, 0x25, 0x00, 0x01, 0xc1, 0x00, 0x00, 0xff, 0x0a}, 4000)...)
	write("package.json", []byte(`{"name":"app"}`))
	write("fixtures/live/segment.ts", segment)
	write("src/app.ts", []byte("export function hello() { return 1 }\n"))
	write("src/sep.ts", []byte("export function sep(a: string[]) { return a.join('\x00') }\n"))

	got, err := New().Extract(context.Background(), dir, []string{"fixtures/live/segment.ts", "src/app.ts", "src/sep.ts"})
	if err != nil {
		t.Fatal(err)
	}
	var hello, sep bool
	for _, f := range got {
		if f.File == "fixtures/live/segment.ts" {
			t.Errorf("fact %s/%s from a binary file", f.Kind, f.Name)
		}
		hello = hello || f.Name == "src.hello"
		sep = sep || f.Name == "src.sep"
	}
	if !hello {
		t.Error("the real TypeScript file produced no symbol")
	}
	if !sep {
		t.Error("a TypeScript file with a NUL separator in a string was skipped as binary")
	}
}
