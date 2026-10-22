package queryloops

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/extractors/goextractor"
	"github.com/enola-labs/enola/internal/facts"
)

// goStore runs the Go extractor over files written as a module and returns its facts.
func goStore(t *testing.T, files map[string]string) *facts.Store {
	t.Helper()
	dir := t.TempDir()
	files["go.mod"] = "module example.com/app\n\ngo 1.22\n"
	var rel []string
	for name, src := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(name, ".go") {
			rel = append(rel, name)
		}
	}
	sort.Strings(rel)
	ff, err := goextractor.New().Extract(context.Background(), dir, rel)
	if err != nil {
		t.Fatal(err)
	}
	store := facts.NewStore()
	for _, f := range ff {
		store.Add(f)
	}
	return store
}

// The Go N+1: a service loops over its input and calls a repository through the
// interface it holds; the repository reads inside a transaction closure. Draining a
// cursor is one query, not one per row, and a call that ignores the element repeats a
// query rather than issuing one per element.
func TestGoQueryLoops(t *testing.T) {
	store := goStore(t, map[string]string{
		"domain/trip/domain.go": `package trip

import "context"

type Event struct{ ID int }

type Repository interface {
	GetPricing(ctx context.Context, id int) (int, error)
	Settings(ctx context.Context) (int, error)
}

type Service struct{ repo Repository }

func (s *Service) GetTrips(ctx context.Context, events []Event) {
	for _, e := range events {
		s.repo.GetPricing(ctx, e.ID)
	}
}

func (s *Service) Totals(ctx context.Context, events []Event) {
	for range events {
		s.repo.Settings(ctx)
	}
}

func (s *Service) Nested(ctx context.Context, groups [][]Event) {
	for _, g := range groups {
		for _, e := range g {
			s.repo.GetPricing(ctx, e.ID)
		}
	}
}
`,
		"adapters/db/trip.go": `package db

import (
	"context"
	"database/sql"

	"example.com/app/domain/trip"
)

type TripRepository struct{ db *sql.DB }

func NewTripRepository(d *sql.DB) trip.Repository { return &TripRepository{db: d} }

func (r *TripRepository) withTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	return fn(tx)
}

func (r *TripRepository) GetPricing(ctx context.Context, id int) (int, error) {
	var p int
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, "select price from trips where id = ?", id).Scan(&p)
	})
	return p, err
}

func (r *TripRepository) Settings(ctx context.Context) (int, error) {
	var v int
	return v, r.db.QueryRowContext(ctx, "select v from settings").Scan(&v)
}

func drain(rows *sql.Rows) []int {
	var out []int
	for rows.Next() {
		var v int
		rows.Scan(&v)
		out = append(out, v)
	}
	return out
}
`,
	})

	byKey := make(map[string]Finding)
	for _, f := range All(store) {
		byKey[f.Symbol+" "+f.Call] = f
	}

	f, ok := byKey["domain/trip.Service.GetTrips domain/trip.Repository.GetPricing"]
	if !ok {
		t.Fatalf("the loop over a repository call through its interface is the N+1; got %v", keysOf(byKey))
	}
	if f.Confidence != 0.7 {
		t.Errorf("a path through a constructor-stated implementer is 0.7, got %v", f.Confidence)
	}
	// The depth is the extractor's scaling depth: a loop over the outer loop's element
	// is bounded by its rules, so this one is reported, at the depth it assigns.
	if _, ok := byKey["domain/trip.Service.Nested domain/trip.Repository.GetPricing"]; !ok {
		t.Errorf("the nested loop should be reported, got %v", keysOf(byKey))
	}
	if inv, ok := byKey["domain/trip.Service.Totals domain/trip.Repository.Settings"]; !ok || inv.Confidence != 0.5 {
		t.Errorf("a call that ignores the element repeats one query: want it at 0.5, got %+v (found %v)", inv, ok)
	}
	for key := range byKey {
		if strings.HasPrefix(key, "adapters/db.drain ") {
			t.Errorf("draining a cursor is one query, not a finding: %s", key)
		}
	}

	insights, err := New().Explain(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range insights {
		if strings.HasPrefix(in.Title, "domain/trip.Service.GetTrips ") {
			if !strings.Contains(in.Description, "adapters/db.TripRepository.GetPricing") ||
				!strings.Contains(in.Description, "database/sql.Tx.QueryRowContext") {
				t.Errorf("the description should print the path to the query: %q", in.Description)
			}
			return
		}
	}
	t.Fatalf("no insight for GetTrips among %d", len(insights))
}

func keysOf(m map[string]Finding) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
