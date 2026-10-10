package rubyextractor

import (
	"os"
	"path/filepath"
	"testing"
)

// `with_options class_name: …` names the class of every association in its block.
// An association that names its own keeps it, and one outside the block is
// derived from its name as before.
func TestAssociationsInsideWithOptionsTakeItsClassName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "account.rb")
	src := `class Account < ApplicationRecord
  has_many :tag_follows, dependent: :destroy

  with_options class_name: 'Block', dependent: :destroy do
    has_many :block_relationships, foreign_key: 'account_id'
    has_many :blocked_by_relationships, foreign_key: :target_account_id
    has_many :reports, class_name: 'Report'
  end

  with_options dependent: :destroy do
    has_many :notes
  end
end
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	got := map[string]modelAssociation{}
	for _, a := range parseModelFile(path) {
		got[a.name] = a
	}
	for name, want := range map[string]struct {
		target string
		via    string
	}{
		"block_relationships":      {"Block", targetDeclared},
		"blocked_by_relationships": {"Block", targetDeclared},
		"reports":                  {"Report", targetDeclared},
		"notes":                    {"Note", targetDerived},
		"tag_follows":              {"TagFollow", targetDerived},
	} {
		a, ok := got[name]
		if !ok {
			t.Errorf("%s: not read", name)
			continue
		}
		if a.target != want.target || a.via != want.via {
			t.Errorf("%s: target %q via %q, want %q via %q", name, a.target, a.via, want.target, want.via)
		}
	}
}
