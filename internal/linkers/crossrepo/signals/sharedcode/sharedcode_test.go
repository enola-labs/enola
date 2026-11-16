package sharedcode

import (
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/linkers/vocab"
	"github.com/enola-labs/enola/pkg/plugin"
)

// fakeInput is a plugin.SignalInput that only answers ReadSource; the file-comparison
// tests need nothing else, and stubbing the rest would assert nothing.
type fakeInput struct {
	plugin.SignalInput
	read func(facts.Fact) (string, bool)
}

func (f fakeInput) ReadSource(fact facts.Fact) (string, bool) { return f.read(fact) }

// symInFile builds a type symbol declared in a specific repo and file.
func symInFile(repo, name, file string) facts.Fact {
	return facts.Fact{Kind: facts.KindSymbol, Name: name, Repo: repo, File: file,
		Props: map[string]any{"symbol_kind": facts.SymbolClass}}
}

func TestIsConventionalComponentName(t *testing.T) {
	s := New(vocab.Default())
	for _, tc := range []struct {
		id   string
		want bool
	}{
		{"SidebarProps", true},
		{"DialogProps", true},
		{"PanelSectionProps", true}, // every segment generic
		{"Layout", true},
		{"CardHeaderState", true},
		{"TileProps", false},       // core shorter than the length floor, still meaningful
		{"GaugePanelProps", false}, // "Pin" is not generic vocabulary
		{"ProbeSlot", false},
		{"TListRow", false},     // single-char prefix must not be read as generic
		{"TestRegistry", false}, // "Names" is not generic vocabulary
		{"WidgetRegistry", false},
		{"HTTPServerProps", false}, // acronym segment kept intact
	} {
		if got := s.isConventionalComponentName(tc.id); got != tc.want {
			t.Errorf("s.isConventionalComponentName(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

func TestSplitCamelCase(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"PanelSection", []string{"Panel", "Section"}},
		{"Footer", []string{"Footer"}},
		{"TListRow", []string{"T", "List", "Row"}},
		{"HTTPServer", []string{"HTTP", "Server"}},
		{"", nil},
	} {
		if got := splitCamelCase(tc.in); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("splitCamelCase(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestIsNonContractSharedFile(t *testing.T) {
	s := New(vocab.Default())
	for _, tc := range []struct {
		file string
		want bool
	}{
		{"db/migrate/20230101_create_widgets.rb", true},
		{"svc/db/migrate/20230101_create_widgets.rb", true},
		{"libs/ui/src/lib/Gallery/Gallery.stories.tsx", true},
		{"client/components/feed/feed.test.tsx", true},
		{"client/components/feed/feed.spec.ts", true},
		{"internal/linkers/crossrepo/crossrepo_test.go", true},
		{"spec/support/matchers/jwt_including_matcher.rb", true},
		{"app/__mocks__/api.ts", true},
		{"app/__tests__/feed.tsx", true},
		{"spec/factories/users.rb", true},
		{"test/fixtures/payload.json", true},
		{"config/initializers/patches/schema_dump.rb", true},
		{"packs/support/config/helpers/request_tags.rb", true},
		{"engines/billing/config/initializers/billing.rb", true},
		{"app/config/settings.rb", false},
		{"libs/ui/src/lib/Gallery/Gallery.tsx", false},
		{"app/models/widget.rb", false},
		{"client/components/latest/index.tsx", false}, // "test" inside a word must not match
		{"", false},
	} {
		if got := s.isNonContractSharedFile(tc.file); got != tc.want {
			t.Errorf("s.isNonContractSharedFile(%q) = %v, want %v", tc.file, got, tc.want)
		}
	}
}

func TestJaccardAndTokenSet(t *testing.T) {
	s := New(vocab.Default())
	if got := jaccard(tokenSet(bodyAlpha), tokenSet(bodyAlphaReformatted)); got != 1.0 {
		t.Errorf("reformatted copy similarity = %v, want 1.0 (whitespace normalized away)", got)
	}
	if got := jaccard(tokenSet(bodyAlpha), tokenSet(bodyBeta)); got >= s.v.Thresholds.MinFileSimilarity {
		t.Errorf("same-name-different-code similarity = %v, want < %v", got, s.v.Thresholds.MinFileSimilarity)
	}
	if got := jaccard(tokenSet(""), tokenSet(bodyAlpha)); got != 0 {
		t.Errorf("empty file similarity = %v, want 0", got)
	}
	// Tokens, not lines: a copy whose lines were each edited slightly is still a copy.
	// Whole-line comparison scored this kind of drift as a near-total mismatch, which
	// was the metric's main failure mode on real repos.
	edited := strings.ReplaceAll(bodyAlpha, "widget", "item")
	if got := jaccard(tokenSet(bodyAlpha), tokenSet(edited)); got < s.v.Thresholds.MinFileSimilarity {
		t.Errorf("within-line drift similarity = %v, want >= %v", got, s.v.Thresholds.MinFileSimilarity)
	}
}

const bodyAlpha = `class WidgetRegistry
  def register(widget)
    @widgets << widget
    recompute_index
  end
  def recompute_index
    @index = @widgets.group_by(&:kind)
  end
end`

// bodyAlphaReformatted is the same code with blank lines and indentation churn: a copy
// that drifted only in formatting must still verify.
const bodyAlphaReformatted = `class WidgetRegistry

    def register(widget)
        @widgets << widget
        recompute_index
    end

    def recompute_index
        @index = @widgets.group_by(&:kind)
    end
end`

// bodyBeta shares the class NAME with bodyAlpha and nothing else — the population the
// verification exists to reject.
const bodyBeta = `class WidgetRegistry
  belongs_to :account
  validates :slug, presence: true, uniqueness: { scope: :account_id }
  scope :active, -> { where(archived_at: nil) }
  def to_param
    slug
  end
end`

// TestFileComparer_ReadsEachFileOnce pins the memoization: several shared identities
// usually resolve to the same file pair, and link time should not scale with them.
func TestFileComparer_ReadsEachFileOnce(t *testing.T) {
	s := New(vocab.Default())
	reads := map[string]int{}
	fc := s.newFileComparer(fakeInput{read: func(f facts.Fact) (string, bool) {
		reads[f.File]++
		return bodyAlpha, true
	}})
	a := symInFile("svc-a", "app.X", "svc-a/app/shared.rb")
	b := symInFile("svc-b", "app.X", "svc-b/app/shared.rb")
	for i := 0; i < 5; i++ {
		if !fc.similar(a, b) {
			t.Fatal("identical files should be similar")
		}
	}
	for file, n := range reads {
		if n != 1 {
			t.Errorf("file %s read %d times, want 1", file, n)
		}
	}
}

// jobsInput is fakeInput reading with a set number of jobs, counting each file's reads.
type jobsInput struct {
	fakeInput
	jobs int
}

func (j jobsInput) ReadJobs() int { return j.jobs }

// TestVerifyPairs_JobsDoNotChangeTheResult pins that verifying the repository pairs in
// parallel keeps and rejects exactly the identities a sequential pass does, records the
// same matched files, and still reads each file once. Run under -race it is also the
// check that the comparer is safe to share.
func TestVerifyPairs_JobsDoNotChangeTheResult(t *testing.T) {
	s := New(vocab.Default())
	repos := []string{"r0", "r1", "r2", "r3", "r4", "r5"}
	// Identity i is declared in every repository; in r0 to r2 the files are copies,
	// in the rest each repository has its own body, so only pairs inside r0..r2 verify.
	idToRepos := map[string]map[string][]facts.Fact{}
	for i := range 40 {
		id := "app.Shared" + string(rune('A'+i%26)) + strings.Repeat("x", i/26)
		idToRepos[id] = map[string][]facts.Fact{}
		for _, r := range repos {
			file := r + "/app/" + id + ".rb"
			idToRepos[id][r] = []facts.Fact{symInFile(r, id, file)}
		}
	}
	body := func(f facts.Fact) string {
		switch f.Repo {
		case "r0", "r1", "r2":
			return bodyAlpha
		}
		return bodyBeta + "\n# " + f.Repo
	}
	run := func(jobs int) (map[string]map[string]bool, map[string][]string, map[string]int) {
		pairShared := map[string]map[string]bool{}
		for a := range repos {
			for b := a + 1; b < len(repos); b++ {
				key := repos[a] + "\x00" + repos[b]
				pairShared[key] = map[string]bool{}
				for id := range idToRepos {
					pairShared[key][id] = true
				}
			}
		}
		var mu sync.Mutex
		reads := map[string]int{}
		in := jobsInput{jobs: jobs, fakeInput: fakeInput{read: func(f facts.Fact) (string, bool) {
			mu.Lock()
			reads[f.File]++
			mu.Unlock()
			return body(f), true
		}}}
		matched := map[string][]string{}
		s.verifyPairs(in, pairShared, idToRepos, matched)
		for k := range matched {
			sort.Strings(matched[k])
		}
		return pairShared, matched, reads
	}
	wantPairs, wantMatched, _ := run(1)
	if len(wantPairs["r0\x00r1"]) != 40 || len(wantPairs["r0\x00r3"]) != 0 {
		t.Fatalf("fixture: r0/r1 verified %d, r0/r3 verified %d; want 40 and 0",
			len(wantPairs["r0\x00r1"]), len(wantPairs["r0\x00r3"]))
	}
	for _, jobs := range []int{2, 8} {
		gotPairs, gotMatched, reads := run(jobs)
		if !reflect.DeepEqual(gotPairs, wantPairs) {
			t.Errorf("jobs=%d: verified identities differ from the sequential pass", jobs)
		}
		if !reflect.DeepEqual(gotMatched, wantMatched) {
			t.Errorf("jobs=%d: matched files differ from the sequential pass", jobs)
		}
		for file, n := range reads {
			if n != 1 {
				t.Errorf("jobs=%d: %s read %d times, want 1", jobs, file, n)
			}
		}
	}
}
