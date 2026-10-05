package resolve

import (
	"strings"
	"testing"
)

func TestParseScopedQuery(t *testing.T) {
	cases := []struct {
		in         string
		repo       string
		kinds      []string
		symbolKind string
		filePrefix string
		term       string
	}{
		{"Currency", "", nil, "", "", "Currency"},
		{"repo:golf kind:struct Currency", "golf", []string{"symbol"}, "struct", "", "Currency"},
		{"kind:module internal/server", "", []string{"module"}, "", "", "internal/server"},
		{"repo:golf/subtenant", "golf", nil, "", "", "subtenant"},
		{"kind:symbol/Currency", "", []string{"symbol"}, "", "", "Currency"},
		{"file:/domain//Currency", "", nil, "", "domain", "Currency"},
		{"file:domain", "", nil, "", "domain", ""},
		{"http://example.com", "", nil, "", "", "http://example.com"}, // not a scope keyword
	}
	for _, tc := range cases {
		sq := parseScopedQuery(tc.in)
		if sq.Repo != tc.repo {
			t.Errorf("%q: Repo = %q, want %q", tc.in, sq.Repo, tc.repo)
		}
		if sq.SymbolKind != tc.symbolKind {
			t.Errorf("%q: SymbolKind = %q, want %q", tc.in, sq.SymbolKind, tc.symbolKind)
		}
		if sq.FilePrefix != tc.filePrefix {
			t.Errorf("%q: FilePrefix = %q, want %q", tc.in, sq.FilePrefix, tc.filePrefix)
		}
		if sq.Term != tc.term {
			t.Errorf("%q: Term = %q, want %q", tc.in, sq.Term, tc.term)
		}
		if strings.Join(sq.Kinds, ",") != strings.Join(tc.kinds, ",") {
			t.Errorf("%q: Kinds = %v, want %v", tc.in, sq.Kinds, tc.kinds)
		}
	}
}

func TestMatchTier_QualifiedSuffix(t *testing.T) {
	// matchTier expects an already-lowercased term (callers lowercase sq.Term).
	if got := matchTier("internal/domain/ticket.Repository", "ticket.repository"); got != 1 {
		t.Errorf("qualified-suffix tier = %d, want 1", got)
	}
	if got := matchTier("internal/adapters/contracts.Repository", "ticket.repository"); got != 0 {
		t.Errorf("non-suffix tier = %d, want 0", got)
	}
	// Suffix must align on a '.'/'/' boundary, not mid-token.
	if got := matchTier("internal/domain/myticket.Repository", "ticket.repository"); got != 0 {
		t.Errorf("mid-token suffix tier = %d, want 0", got)
	}
}
