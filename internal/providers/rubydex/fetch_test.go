package rubydex

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPins_CoverEveryPublishedPlatform(t *testing.T) {
	for _, key := range []string{"linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"} {
		pin, ok := pins[key]
		if !ok || len(pin.sha256) != 64 || pin.platform == "" {
			t.Errorf("%s: the pin must name the gem platform and a full sha256, got %+v", key, pin)
		}
	}
}

func gemWith(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var data bytes.Buffer
	gz := gzip.NewWriter(&data)
	inner := tar.NewWriter(gz)
	for name, body := range files {
		if err := inner.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := inner.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := inner.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	var gem bytes.Buffer
	outer := tar.NewWriter(&gem)
	for name, body := range map[string][]byte{"metadata.gz": []byte("x"), "data.tar.gz": data.Bytes()} {
		if err := outer.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := outer.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := outer.Close(); err != nil {
		t.Fatal(err)
	}
	return gem.Bytes()
}

// A gem is a tar around a gzipped tar; the library sits at lib/rubydex/ under
// the platform's file name, and nothing else in the gem is read.
func TestExtractLibrary_ReadsTheEngineOutOfTheGem(t *testing.T) {
	gem := gemWith(t, map[string]string{
		"lib/rubydex.rb":                   "ruby",
		"lib/rubydex/" + libraryFileName(): "ELF-or-Mach-O",
	})
	got, err := extractLibrary(gem)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "ELF-or-Mach-O" {
		t.Fatalf("extracted %q", got)
	}
	if _, err := extractLibrary(gemWith(t, map[string]string{"lib/rubydex.rb": "ruby"})); err == nil || !strings.Contains(err.Error(), "carries no lib/rubydex/") {
		t.Fatalf("a gem without the library must be refused by name, got %v", err)
	}
}

func fetchAgainst(t *testing.T, srv *httptest.Server) {
	t.Helper()
	prev := downloadBase
	downloadBase = srv.URL + "/"
	t.Cleanup(func() { downloadBase = prev })
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Setenv("HOME", cache)
}

func TestFetch_AStalledDownloadEndsAtTheTimeout(t *testing.T) {
	if _, ok := pins[platformKey()]; !ok {
		t.Skip("no gem is published for this platform")
	}
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000000")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	fetchAgainst(t, srv)
	prev := fetchTimeout
	fetchTimeout = 300 * time.Millisecond
	t.Cleanup(func() { fetchTimeout = prev })

	done := make(chan error, 1)
	go func() {
		_, err := Fetch(context.Background(), nil)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a download that stalled after its headers must fail")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the fetch is still waiting on a stalled body: it has no overall timeout")
	}
}
