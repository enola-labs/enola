package check

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

const (
	gradedSource = "package customers\n\nfunc Erase(subject string) {\n\tlog.Printf(\"erasing %s\", subject)\n}\n"
	otherSource  = "package customers\n\n// an older checkout\n}\n"
)

// writeRepo creates dir/rel with content and returns dir.
func writeRepo(t *testing.T, dir, rel, content string) string {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func sha(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// metas returns a metaFor serving one meta per directory.
func metas(byDir map[string]facts.SnapshotMeta) func(string) facts.SnapshotMeta {
	return func(dir string) facts.SnapshotMeta { return byDir[dir] }
}

func located(file string, line int) facts.Insight {
	return facts.Insight{Title: "forbidden call", Source: "constraints", Confidence: 1,
		Evidence: []facts.Evidence{{File: file, Detail: "forbidden calls edge", Line: line}}}
}

// The bug this type exists for: a check run on a repository from a directory
// holding another checkout with the same file names quoted the other checkout.
func TestFrame_QuotesTheGradedRepositoryNotTheWorkingDirectory(t *testing.T) {
	repo := writeRepo(t, t.TempDir(), "customers/store.go", gradedSource)
	elsewhere := writeRepo(t, t.TempDir(), "customers/store.go", otherSource)
	t.Chdir(elsewhere)

	v := AttachSources(Verdict{Failures: []facts.Insight{located("customers/store.go", 4)}},
		[]string{repo}, metas(map[string]facts.SnapshotMeta{repo: {RepoLabel: "shop",
			FileHashes: []facts.FileHash{{Path: "customers/store.go", Hash: sha(gradedSource)}}}}))

	var sb strings.Builder
	v.writeFindings(&sb, v.Failures)
	out := sb.String()
	if !strings.Contains(out, `log.Printf("erasing %s", subject)`) {
		t.Fatalf("the frame must quote the graded repository:\n%s", out)
	}
	if strings.Contains(out, "\n      }\n") {
		t.Fatalf("the frame quoted the working directory's checkout:\n%s", out)
	}
}

// Run from a directory that holds nothing, the frame is still there.
func TestFrame_ShownFromAnUnrelatedDirectory(t *testing.T) {
	repo := writeRepo(t, t.TempDir(), "customers/store.go", gradedSource)
	t.Chdir(t.TempDir())

	v := AttachSources(Verdict{}, []string{repo}, metas(map[string]facts.SnapshotMeta{repo: {RepoLabel: "shop"}}))
	var sb strings.Builder
	v.writeFindings(&sb, []facts.Insight{located("customers/store.go", 3)})
	if !strings.Contains(sb.String(), "func Erase(subject string) {") {
		t.Fatalf("the frame must be read from the repository, wherever the check runs:\n%s", sb.String())
	}
}

// A file that changed after it was graded prints no frame; one the snapshot
// never hashed is read as it is.
func TestFrame_OnlyQuotesTheContentThatWasGraded(t *testing.T) {
	repo := writeRepo(t, t.TempDir(), "customers/store.go", otherSource)
	writeRepo(t, repo, "enola/constraints/gdpr.yaml", "rules:\n  - id: r\n")
	v := AttachSources(Verdict{}, []string{repo}, metas(map[string]facts.SnapshotMeta{repo: {RepoLabel: "shop",
		FileHashes: []facts.FileHash{{Path: "customers/store.go", Hash: sha(gradedSource)}}}}))

	var sb strings.Builder
	v.writeFindings(&sb, []facts.Insight{located("customers/store.go", 3)})
	if strings.Contains(sb.String(), "customers/store.go:3") {
		t.Fatalf("a file edited after grading must print no frame:\n%s", sb.String())
	}

	sb.Reset()
	v.writeFindings(&sb, []facts.Insight{located("enola/constraints/gdpr.yaml", 2)})
	if !strings.Contains(sb.String(), "  - id: r") {
		t.Fatalf("an unhashed file must still be quoted:\n%s", sb.String())
	}
}

// In a cluster, the label a file carries picks the repository it is read from
// and the path a host is given, whatever the working directory holds.
func TestSources_ClusterResolvesByLabel(t *testing.T) {
	root := t.TempDir()
	api := writeRepo(t, filepath.Join(root, "svc-api"), "server.go", "package main\n\nfunc getOrder() {}\n")
	web := writeRepo(t, filepath.Join(root, "svc-web"), "client.go", "package main\n\nfunc fetchOrder() {}\n")
	// A decoy in the working directory, spelled the way an unanchored read resolved it.
	writeRepo(t, root, "api/server.go", "package decoy\n\nfunc wrong() {}\n")
	t.Chdir(root)

	v := AttachSources(Verdict{}, []string{api, web}, metas(map[string]facts.SnapshotMeta{
		api: {RepoLabel: "api"}, web: {RepoLabel: "web"}}))

	var sb strings.Builder
	v.writeFindings(&sb, []facts.Insight{located("api/server.go", 3), located("web/client.go", 3)})
	out := sb.String()
	if !strings.Contains(out, "func getOrder() {}") || !strings.Contains(out, "func fetchOrder() {}") {
		t.Fatalf("each frame must come from its own member:\n%s", out)
	}
	if strings.Contains(out, "func wrong()") {
		t.Fatalf("a frame was read from the working directory:\n%s", out)
	}

	if got := v.sources.hostPath("api/server.go"); got != "svc-api/server.go" {
		t.Errorf("hostPath from the cluster's parent = %q, want svc-api/server.go", got)
	}
	t.Chdir(t.TempDir())
	if got := v.sources.hostPath("web/client.go"); got != "client.go" {
		t.Errorf("hostPath from outside every member = %q, want the member-relative client.go", got)
	}
	if got := v.sources.hostPath("billing/invoice.go"); got != "billing/invoice.go" {
		t.Errorf("a label no member carries must be printed as recorded, got %q", got)
	}
	sb.Reset()
	v.writeFindings(&sb, []facts.Insight{located("billing/invoice.go", 1)})
	if strings.Contains(sb.String(), "billing/invoice.go:1") {
		t.Fatalf("a file no member claims must print no frame:\n%s", sb.String())
	}
}

// Run from inside a lone repository, hostPath is the path as recorded, which is
// what an annotation in CI needs; a directory named like the repository's label
// is a directory, not a label.
func TestSources_SingleRepositoryHostPath(t *testing.T) {
	repo := writeRepo(t, t.TempDir(), "api/server.go", "package api\n")
	t.Chdir(repo)
	v := AttachSources(Verdict{}, []string{repo}, metas(map[string]facts.SnapshotMeta{repo: {RepoLabel: "api",
		FileHashes: []facts.FileHash{{Path: "api/server.go", Hash: sha("package api\n")}}}}))
	if got := v.sources.hostPath("api/server.go"); got != "api/server.go" {
		t.Errorf("hostPath = %q, want api/server.go", got)
	}
}

// The grading machine's directories never reach the JSON verdict.
func TestSources_StayOutOfTheJSONVerdict(t *testing.T) {
	repo := t.TempDir()
	v := AttachSources(Verdict{Status: StatusClean}, []string{repo},
		metas(map[string]facts.SnapshotMeta{repo: {RepoLabel: "shop"}}))
	out, err := v.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), repo) {
		t.Fatalf("the JSON verdict leaked a local path:\n%s", out)
	}
}

// A working directory reached through a symlink is still the directory the
// repository lives under: from its parent, the host path keeps the repository's
// directory rather than falling back to the repository-relative path.
func TestSources_HostPathThroughASymlinkedDirectory(t *testing.T) {
	real := t.TempDir()
	repo := writeRepo(t, filepath.Join(real, "shop"), "customers/store.go", gradedSource)
	link := filepath.Join(t.TempDir(), "via-link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	t.Chdir(link)
	v := AttachSources(Verdict{}, []string{repo}, metas(map[string]facts.SnapshotMeta{repo: {RepoLabel: "shop"}}))
	if got := v.sources.hostPath("customers/store.go"); got != "shop/customers/store.go" {
		t.Errorf("hostPath = %q, want shop/customers/store.go", got)
	}
}
