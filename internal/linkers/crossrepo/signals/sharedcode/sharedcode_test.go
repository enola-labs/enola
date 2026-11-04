package sharedcode

import (
	"reflect"
	"strings"
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
