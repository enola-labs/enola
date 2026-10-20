package providers

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/providers/rubydex"
	"github.com/enola-labs/enola/internal/providerspec"
)

// prism is a name the binary answers in-process: an entry with no command
// validates, and the built-ins named in a refusal include it.
func TestValidate_PrismIsABuiltInName(t *testing.T) {
	if err := providerspec.Validate([]providerspec.Spec{{Name: "prism"}}); err != nil {
		t.Fatalf("prism needs no command: %v", err)
	}
	err := providerspec.Validate([]providerspec.Spec{{Name: "nope"}})
	if err == nil || !strings.Contains(err.Error(), "built-ins: prism, rubydex") {
		t.Fatalf("the refusal names both built-ins, got %v", err)
	}
}

// One engine behind two names is one reader: where its two names emit the same
// call site the seam does not stamp it as agreed by two producers, and keeps
// both relations as emitted.
func TestPairing_OneEngineBehindTwoNamesIsNoAgreement(t *testing.T) {
	prism := `{"kind":"dependency","name":"prism-call: ReminderJob#perform -> Board.find","file":"app/jobs/reminder_job.rb","line":3,` +
		`"props":{"resolution_level":"constant-receiver","engine":"rubydex"},"relations":[{"kind":"calls","target":"Board.find"}]}`
	ff, records := runTwo(t, prism,
		callLine("rubydex-call: ReminderJob#perform -> Board.find", "app/jobs/reminder_job.rb", 3, "Board.find"))
	if len(ff) != 2 {
		t.Fatalf("both relations stay as emitted, got %d: %+v", len(ff), ff)
	}
	for _, f := range ff {
		if _, stamped := f.Props[PropResolutionAgreement]; stamped {
			t.Errorf("%s: one engine read the site once, yet it carries an agreement", f.Name)
		}
	}
	for _, r := range records {
		if r.Agreed != 0 || r.Differing != 0 {
			t.Errorf("%s: agreed %d, differing %d, want neither", r.Name, r.Agreed, r.Differing)
		}
	}
}

// Both names served by the built-in in one run: one index, read once, and the
// facts of each name stamped with that name and the engine's version.
func TestRun_PrismAndRubydexBuiltInsShareOneEngine(t *testing.T) {
	if _, installed := rubydex.Installed(); !installed {
		t.Skip("the Rubydex library is not installed here; the built-in provider cannot run")
	}
	root, files, hashes := writeRubyWorkspace(t)
	writeIn(t, root, "app/invoice.rb", "class Invoice\n  def total\n    sum\n  end\n  def sum; end\nend\n")
	files = append(files, "app/invoice.rb")
	in := Input{Providers: []Provider{{Name: "prism"}, {Name: "rubydex"}}, RepoPath: root, Files: files, Hashes: hashes}
	ff, records := RunWith(context.Background(), in)
	if len(records) != 2 {
		t.Fatalf("records = %+v", records)
	}
	for _, r := range records {
		if r.Skipped || r.Version != rubydex.Version || r.Census == nil {
			t.Fatalf("%s = %+v, want a run of the engine at %s with a census", r.Name, r, rubydex.Version)
		}
		if r.Agreed != 0 {
			t.Errorf("%s: %d agreements between one engine's two names", r.Name, r.Agreed)
		}
	}
	byProvider := map[string][]string{}
	for _, f := range ff {
		provider, _ := f.Props[PropProvider].(string)
		byProvider[provider] = append(byProvider[provider], f.Name)
		if provider == "prism" && (f.Props["engine"] != "rubydex" || !strings.HasPrefix(f.Name, "prism-")) {
			t.Errorf("a prism fact from the built-in names its engine and keeps the script's prefix: %+v", f)
		}
	}
	if len(byProvider["rubydex"]) == 0 {
		t.Errorf("the Rubydex facts are still written: %v", byProvider)
	}
	if strings.Join(byProvider["prism"], "|") != "prism-call: Invoice#total -> Invoice#sum" {
		t.Errorf("the prism name writes the one same-class call: %v", byProvider["prism"])
	}
}

// Under a cache each name is its own entry, and a warm run serves both.
func TestRun_PrismBuiltInIsServedFromItsOwnIndexEntry(t *testing.T) {
	if _, installed := rubydex.Installed(); !installed {
		t.Skip("the Rubydex library is not installed here; the built-in provider cannot run")
	}
	root, files, hashes := writeRubyWorkspace(t)
	cache := newMemoryCache()
	in := Input{Providers: []Provider{{Name: "prism"}, {Name: "rubydex"}}, RepoPath: root, Files: files, Hashes: hashes, Cache: cache}
	cold, _ := RunWith(context.Background(), in)
	warm, records := RunWith(context.Background(), in)
	for _, r := range records {
		if r.Reuse == nil || r.Reuse.Cache != "hit" {
			t.Fatalf("%s: reuse %+v, want a hit", r.Name, r.Reuse)
		}
	}
	if len(cold) != len(warm) {
		t.Fatalf("cold %d facts, warm %d", len(cold), len(warm))
	}
}

func writeIn(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
