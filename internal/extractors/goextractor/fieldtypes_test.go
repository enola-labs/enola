package goextractor

import (
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// A struct field USES every named type its type expression mentions, whatever
// wraps it. A struct reached only as `[]T` or `map[K]T` used to get no edge and
// was reported as dead code.
func TestStructFieldEdgesReachTypesInsideCompositeTypes(t *testing.T) {
	got := extractAll(t, map[string]string{
		"model/model.go": `package model

import "testmod/lib/other"

type Layer struct{}
type Seam struct{}
type Key struct{}
type Value struct{}
type Event struct{}
type Arg struct{}
type Result struct{}
type Wrapped struct{}
type Fixed struct{}
type Box[T any] struct{ v T }

type Config struct {
	Layers   []Layer
	Seams    []*Seam
	Index    map[Key]*Value
	Events   chan Event
	Hook     func(Arg) Result
	Boxed    Box[Wrapped]
	Grid     [4]Fixed
	External []other.Thing
}
`,
		"lib/other/other.go": `package other

type Thing struct{}
`,
	})
	cfg, ok := findFact(got, "model.Config")
	if !ok {
		t.Fatal("model.Config not extracted")
	}
	for _, want := range []string{
		"model.Layer", "model.Seam", "model.Key", "model.Value", "model.Event",
		"model.Arg", "model.Result", "model.Box", "model.Wrapped", "model.Fixed",
		"lib/other.Thing",
	} {
		if !hasRelation(cfg, facts.RelInstantiates, want) {
			t.Errorf("model.Config should use %s through a field type, got %+v", want, cfg.Relations)
		}
	}
}

// A field of a predeclared type, of a type parameter, or of a type with no name
// uses nothing this module declares, so it carries no edge.
func TestStructFieldEdgesNameOnlyDeclaredTypes(t *testing.T) {
	got := extractAll(t, map[string]string{
		"model/model.go": `package model

type Row[T any] struct {
	Name   string
	Count  int
	Err    error
	Tags   []string
	Lookup map[string]bool
	Done   chan struct{}
	Item   T
	Items  []T
	Fn     func(int) error
}
`,
	})
	row, ok := findFact(got, "model.Row")
	if !ok {
		t.Fatal("model.Row not extracted")
	}
	for _, rel := range row.Relations {
		if rel.Kind == facts.RelInstantiates {
			t.Errorf("model.Row has a field edge to %q, which names no declared type", rel.Target)
		}
	}
}

// A bare call through a parameter, a closure or a local is not a call to a package
// function of that name.
func TestBareCallThroughLocalFuncValueIsNotAPackageCall(t *testing.T) {
	got := extractAll(t, map[string]string{
		"svc/svc.go": `package svc

type ID int

var hook = func(int) {}

func helper(n int) int { return n }

func Run(xs []int, visit func(int) bool) int {
	add := func(n int) int { return helper(n) }
	total := 0
	for _, x := range xs {
		if visit(x) {
			total += add(x)
		}
	}
	hook(total)
	return int(ID(total))
}
`,
	})
	run, ok := findFact(got, "svc.Run")
	if !ok {
		t.Fatal("svc.Run not extracted")
	}
	for _, phantom := range []string{"svc.visit", "svc.add"} {
		if hasRelation(run, facts.RelCalls, phantom) {
			t.Errorf("svc.Run records a call to %s, which is a parameter or local, not a function", phantom)
		}
		for _, prop := range []string{"calls_in_loop", "calls_in_scaling_loop", "calls_on_loop_element"} {
			if list, _ := run.PropAny(prop).([]string); strings.Contains(strings.Join(list, " "), phantom) {
				t.Errorf("svc.Run lists %s in %s", phantom, prop)
			}
		}
	}
	// What the package does declare keeps its edge: a function reached from the
	// closure body, a package-level function variable, a conversion to a named type.
	for _, want := range []string{"svc.helper", "svc.hook", "svc.ID"} {
		if !hasRelation(run, facts.RelCalls, want) {
			t.Errorf("svc.Run should still record %s, got %+v", want, run.Relations)
		}
	}
}
