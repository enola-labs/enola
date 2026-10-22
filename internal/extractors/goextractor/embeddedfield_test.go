package goextractor

import (
	"slices"
	"testing"
)

// An embedded field is named by its type, so `r.Base.Query()` is the promoted
// `r.Query()` spelled out. Embedded fields have no names in the AST and were left
// out of the field map, so the explicit form resolved to nothing and a query made
// through it reached no database: the loop around it was no N+1.
func TestEmbeddedFieldCallResolvesThroughItsType(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"internal/base/base.go": `package base

import "database/sql"

type Base struct{ db *sql.DB }

func (b Base) Query(q string) (*sql.Rows, error) { return b.db.Query(q) }
`,
		"internal/repo/repo.go": `package repo

import "testmod/internal/base"

type Repo struct {
	base.Base
}

func (r *Repo) Explicit() { r.Base.Query("SELECT 1") }

func (r *Repo) Promoted() { r.Query("SELECT 1") }
`,
	})
	promoted := callTargetsOf(ff, "internal/repo.Repo.Promoted")
	explicit := callTargetsOf(ff, "internal/repo.Repo.Explicit")
	if !slices.Contains(explicit, "internal/base.Base.Query") {
		t.Errorf("Explicit calls %v, want internal/base.Base.Query (Promoted calls %v)", explicit, promoted)
	}
}
