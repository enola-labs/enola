package goextractor

import (
	"slices"
	"testing"
)

// A repository handed out through a constructor that returns the interface, with
// no assertion anywhere, the way most Go services are written. The service's loop
// calls the interface, and the I/O has to reach it through the constructor's
// statement of what implements it.
func TestImplementers_StatedByAConstructor(t *testing.T) {
	syms := extractGoModule(t, map[string]string{
		"domain/trip/domain.go": `package trip

import "context"

type Repository interface {
	GetPricing(ctx context.Context, id int) (int, error)
	List(ctx context.Context) ([]int, error)
}

type Service struct{ repo Repository }

func (s *Service) GetTrips(ctx context.Context, ids []int) {
	for _, id := range ids {
		s.repo.GetPricing(ctx, id)
	}
}
`,
		"adapters/db/base.go": `package db

import (
	"context"
	"database/sql"
)

type BaseRepository struct{ db *sql.DB }

func (b *BaseRepository) WithTransaction(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	return fn(tx)
}
`,
		"adapters/db/trip.go": `package db

import (
	"context"
	"database/sql"

	"example.com/app/domain/trip"
)

type TripRepository struct{ BaseRepository }

func NewTripRepository(d *sql.DB) trip.Repository { return &TripRepository{BaseRepository{db: d}} }

func (r *TripRepository) GetPricing(ctx context.Context, id int) (int, error) {
	var p int
	err := r.WithTransaction(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, "select price from trips where id = ?", id).Scan(&p)
	})
	return p, err
}

func (r *TripRepository) List(ctx context.Context) ([]int, error) { return nil, nil }
`,
		"mocks/trip_mock.go": `package mocks

import "context"

type MockRepository struct{}

func (m *MockRepository) GetPricing(ctx context.Context, id int) (int, error) { return 0, nil }
func (m *MockRepository) List(ctx context.Context) ([]int, error)              { return nil, nil }
`,
	})

	iface := syms["domain/trip.Repository"]
	got, _ := iface.PropAny(propImplementers).([]string)
	if !slices.Equal(got, []string{"adapters/db.TripRepository"}) {
		t.Errorf("implementers of trip.Repository = %v, want the constructor's type and no mock", got)
	}
	if inferred, _ := iface.PropAny(propImplementersInferred).([]string); slices.Contains(inferred, "mocks.MockRepository") {
		t.Errorf("a mock must not be inferred as an implementer: %v", inferred)
	}
	if !goPerformsIO(syms["domain/trip.Repository.GetPricing"]) {
		t.Error("the interface method should reach the repository's query through the constructor")
	}
	if !goPerformsIO(syms["domain/trip.Service.GetTrips"]) {
		t.Error("the service loop should reach the query through the interface")
	}
	repo := syms["adapters/db.TripRepository.GetPricing"]
	if db, _ := repo.PropAny("db_direct").(bool); !db {
		t.Errorf("a read inside a WithTransaction closure is the method's own database call: %v", repo.Props)
	}
}

// Without a constructor or an assertion, a type whose methods cover an interface of
// two or more methods is inferred to implement it. A one-method interface two types
// satisfy is not: that match says nothing.
func TestImplementers_InferredByMethodSet(t *testing.T) {
	syms := extractGoModule(t, map[string]string{
		"store/store.go": `package store

type Store interface {
	Get(id int) int
	Put(id, v int)
}

type Closer interface{ Close() error }

type memStore struct{}

func (m *memStore) Get(id int) int { return 0 }
func (m *memStore) Put(id, v int)  {}
func (m *memStore) Close() error   { return nil }

type file struct{}

func (f *file) Close() error { return nil }
`,
	})
	if got, _ := syms["store.Store"].PropAny(propImplementersInferred).([]string); !slices.Equal(got, []string{"store.memStore"}) {
		t.Errorf("Store inferred implementers = %v, want memStore", got)
	}
	if got, _ := syms["store.Closer"].PropAny(propImplementersInferred).([]string); len(got) != 0 {
		t.Errorf("a one-method interface two types satisfy must not be inferred: %v", got)
	}
}

// Draining a cursor is reading a result already fetched. Its Next and Scan are pure
// calls, not database calls, so a loop over them is one query and not one per row.
func TestCursorReadsArePureCalls(t *testing.T) {
	syms := extractGoModule(t, map[string]string{
		"repo/repo.go": `package repo

import "database/sql"

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
	f := syms["repo.drain"]
	pure, _ := f.PropAny("pure_calls").([]string)
	if !slices.Contains(pure, "database/sql.Rows.Next") || !slices.Contains(pure, "database/sql.Rows.Scan") {
		t.Errorf("pure_calls = %v, want the cursor's Next and Scan", pure)
	}
	if db, _ := f.PropAny("db_direct").(bool); db {
		t.Error("draining a cursor is not a database call")
	}
}

// Repositories with the same CRUD method names and arity for different entities used
// to match each other's interfaces. The parameter types tell them apart, and a mock
// asserted to implement an interface is not recorded as an implementer.
func TestImplementers_InferredByTypesNotArity(t *testing.T) {
	syms := extractGoModule(t, map[string]string{
		"model/model.go": `package model

type Brand struct{}
type Item struct{}
`,
		"contracts/contracts.go": `package contracts

import "example.com/app/model"

type BrandService interface {
	Get(id int) (*model.Brand, error)
	Save(b *model.Brand) error
}
`,
		"db/repos.go": `package db

import m "example.com/app/model"

type brandRepository struct{}

func (r *brandRepository) Get(id int) (*m.Brand, error) { return nil, nil }
func (r *brandRepository) Save(b *m.Brand) error        { return nil }

type itemRepository struct{}

func (r *itemRepository) Get(id int) (*m.Item, error) { return nil, nil }
func (r *itemRepository) Save(i *m.Item) error        { return nil }
`,
		"mocks/brand.go": `package mocks

import (
	"example.com/app/contracts"
	"example.com/app/model"
)

var _ contracts.BrandService = (*MockBrandService)(nil)

type MockBrandService struct{}

func (s *MockBrandService) Get(id int) (*model.Brand, error) { return nil, nil }
func (s *MockBrandService) Save(b *model.Brand) error        { return nil }
`,
	})
	iface := syms["contracts.BrandService"]
	if got, _ := iface.PropAny(propImplementersInferred).([]string); !slices.Equal(got, []string{"db.brandRepository"}) {
		t.Errorf("BrandService inferred implementers = %v, want only the brand repository", got)
	}
	if got, _ := iface.PropAny(propImplementers).([]string); len(got) != 0 {
		t.Errorf("a mock's assertion must not make it a stated implementer: %v", got)
	}
}
