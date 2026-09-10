package upgrade

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/version"
)

func TestVerifyChecksum(t *testing.T) {
	tarball := []byte("fake tarball contents")
	sum := sha256.Sum256(tarball)
	good := fmt.Sprintf("%s  enola-1.2.3-linux-amd64.tar.gz\n", hex.EncodeToString(sum[:]))

	if err := verifyChecksum(tarball, []byte(good)); err != nil {
		t.Fatalf("expected valid checksum to pass, got %v", err)
	}

	// Tampered payload must fail against the same recorded digest.
	if err := verifyChecksum([]byte("tampered"), []byte(good)); err == nil {
		t.Fatal("expected checksum mismatch, got nil")
	}

	if err := verifyChecksum(tarball, []byte("")); err == nil {
		t.Fatal("expected error on empty checksum file, got nil")
	}
}

func TestAssetNames(t *testing.T) {
	cases := []struct {
		goos, goarch string
		wantTarball  string
		wantInner    string
	}{
		{"linux", "amd64", "enola-1.2.3-linux-amd64.tar.gz", "enola-1.2.3-linux-amd64"},
		{"linux", "arm64", "enola-1.2.3-linux-arm64.tar.gz", "enola-1.2.3-linux-arm64"},
		{"darwin", "amd64", "enola-1.2.3-darwin-amd64.tar.gz", "enola-1.2.3-darwin-amd64"},
		{"darwin", "arm64", "enola-1.2.3-darwin-arm64.tar.gz", "enola-1.2.3-darwin-arm64"},
		{"windows", "amd64", "enola-1.2.3-windows-amd64.tar.gz", "enola-1.2.3-windows-amd64.exe"},
	}
	for _, c := range cases {
		got, err := assetNames("1.2.3", c.goos, c.goarch)
		if err != nil {
			t.Fatalf("%s/%s: unexpected error %v", c.goos, c.goarch, err)
		}
		if got.tarball != c.wantTarball {
			t.Errorf("%s/%s tarball = %q, want %q", c.goos, c.goarch, got.tarball, c.wantTarball)
		}
		// Not install.sh's `.sha256`: a shared asset would make upgrades
		// indistinguishable from installs in the download counters.
		if got.checksum != "enola-1.2.3-"+c.goos+"-"+c.goarch+".upgrade.sha256" {
			t.Errorf("%s/%s checksum = %q", c.goos, c.goarch, got.checksum)
		}
		if got.innerBinary != c.wantInner {
			t.Errorf("%s/%s inner = %q, want %q", c.goos, c.goarch, got.innerBinary, c.wantInner)
		}
	}

	if _, err := assetNames("1.2.3", "linux", "riscv64"); err == nil {
		t.Fatal("expected error for unsupported platform, got nil")
	}
}

func TestExtractBinary(t *testing.T) {
	want := []byte("#!/bin/sh\necho hi\n")
	tarball := buildTarGz(t, "enola-1.2.3-linux-amd64", want)

	got, err := extractBinary(tarball, "enola-1.2.3-linux-amd64")
	if err != nil {
		t.Fatalf("extractBinary: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("extracted %q, want %q", got, want)
	}

	if _, err := extractBinary(tarball, "not-present"); err == nil {
		t.Fatal("expected error for missing entry, got nil")
	}
}

func TestLatestVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/enola-labs/enola/releases/latest" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprint(w, `{"tag_name": "v0.1.26", "name": "release"}`)
	}))
	defer srv.Close()

	old := apiBase
	apiBase = srv.URL
	defer func() { apiBase = old }()

	got, err := latestVersion(context.Background())
	if err != nil {
		t.Fatalf("latestVersion: %v", err)
	}
	if got != "0.1.26" {
		t.Errorf("latestVersion = %q, want %q", got, "0.1.26")
	}
}

func TestDownload(t *testing.T) {
	body := []byte("asset bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	got, err := download(context.Background(), srv.URL+"/enola.tar.gz")
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("download = %q, want %q", got, body)
	}

	// Non-2xx must surface as an error.
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer fail.Close()
	if _, err := download(context.Background(), fail.URL); err == nil {
		t.Fatal("expected error on 404, got nil")
	}
}

// buildTarGz returns a gzipped tar archive containing a single file.
// TestExtractBinaryIgnoresAccompanyingFiles pins that `enola upgrade` survives a
// release tarball carrying more than the binary.
//
// The tarball ships LICENSE and NOTICE beside the executable, because the vendored
// tree-sitter grammars are MIT and the notice has to travel with the copy people
// actually download. An upgrade path that assumed a single-member archive — or that
// took the first entry — would break on the first release that did so, and it would
// break in the field rather than in CI.
//
// LICENSE is deliberately placed FIRST, which is the case a "read the first member"
// implementation would pass by accident if the binary came first.
func TestExtractBinaryIgnoresAccompanyingFiles(t *testing.T) {
	want := []byte("#!/bin/sh\necho hi\n")
	tarball := buildTarGzWith(t, []tarMember{
		{name: "LICENSE", data: []byte("Apache License 2.0 ...")},
		{name: "NOTICE", data: []byte("tree-sitter-swift ... tree-sitter-dart ...")},
		{name: "enola-1.2.3-linux-amd64", data: want},
	})

	got, err := extractBinary(tarball, "enola-1.2.3-linux-amd64")
	if err != nil {
		t.Fatalf("extractBinary: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("extracted %q, want the binary %q", got, want)
	}

	// And the accompanying files are reachable by name, so a future `--licenses`
	// could read them from the same archive rather than needing a second download.
	notice, err := extractBinary(tarball, "NOTICE")
	if err != nil {
		t.Fatalf("NOTICE should be present in the archive: %v", err)
	}
	if !bytes.Contains(notice, []byte("tree-sitter-dart")) {
		t.Error("NOTICE should attribute the vendored Dart grammar")
	}
}

// buildTarGzWith builds an archive with several members, in the order given. It
// mirrors the real release tarball, which carries LICENSE and NOTICE beside the
// binary so the vendored grammars' MIT notice travels with the copy people install.
func buildTarGzWith(t *testing.T, members []tarMember) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, m := range members {
		hdr := &tar.Header{Name: m.name, Mode: 0o644, Size: int64(len(m.data))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(m.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type tarMember struct {
	name string
	data []byte
}

func buildTarGz(t *testing.T, name string, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	hdr := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(data))}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestExternallyManagedRefusesAPipInstall pins the guard that keeps `enola
// upgrade` from fighting pip for ownership of the binary.
//
// The failure this prevents is not a crash. Replacing the file WORKS, once: the
// user gets the new version and nothing complains, and then some later pip
// operation quietly puts the old one back. So the test asserts the two things
// that make the guard useful rather than merely present, namely that it names
// the command that does work, and that it names the PyPI project rather than the
// command name (`enola-cli` and `enola` differ, because `enola` was taken).
func TestExternallyManagedRefusesAPipInstall(t *testing.T) {
	if msg := externallyManaged(); msg != "" {
		t.Fatalf("a source build must be free to self-update, got:\n%s", msg)
	}

	prev := version.InstallMethod
	version.InstallMethod = "pip"
	t.Cleanup(func() { version.InstallMethod = prev })

	msg := externallyManaged()
	if msg == "" {
		t.Fatal("expected a pip install to be refused")
	}
	if !strings.Contains(msg, "pip install -U enola-cli") {
		t.Errorf("message does not name the command that works:\n%s", msg)
	}
	if strings.Contains(msg, "enola upgrade") {
		t.Errorf("message points back at the command being refused:\n%s", msg)
	}
}

// TestRunOnAPipInstallNeverReachesTheNetwork pins WHERE the guard sits.
//
// Checking after the release lookup would still refuse the upgrade, so a test
// that only asserted the refusal would pass on a version of this that spends a
// GitHub API round trip first. It would also inherit that call's failure modes:
// offline, or rate limited by a shared IP, would turn a message the user needs
// into an error about something irrelevant.
func TestRunOnAPipInstallNeverReachesTheNetwork(t *testing.T) {
	prev := version.InstallMethod
	version.InstallMethod = "pip"
	t.Cleanup(func() { version.InstallMethod = prev })

	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("pip install still contacted the network: %s", r.URL)
	}))
	defer srv.Close()

	prevAPI, prevDL := apiBase, downloadBase
	apiBase, downloadBase = srv.URL, srv.URL
	t.Cleanup(func() { apiBase, downloadBase = prevAPI, prevDL })

	if err := Run(context.Background(), "0.4.12"); err != nil {
		t.Fatalf("refusing to self-update is not an error condition, got %v", err)
	}
}
