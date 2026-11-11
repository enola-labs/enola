package impact

import (
	"slices"
	"testing"
)

// A router mounted at a path, its handler held in a variable, is left out when an
// endpoint under that path in the same repository is already listed more precisely.
// It stays when nothing under it is, when the entry under it is itself only by
// package, and when the precise entry is in another repository.
func TestDropCoveredMounts(t *testing.T) {
	out := dropCoveredMounts([]Endpoint{
		{Method: "GET", Path: "/shelves", Repo: "library", Precision: PrecisionInterface},
		{Method: "ALL", Path: "/", Repo: "library", Precision: PrecisionPackage, HandlerUnresolved: true},
		{Method: "ALL", Path: "/admin", Repo: "library", Precision: PrecisionPackage, HandlerUnresolved: true},
		{Method: "GET", Path: "/admin/stats", Repo: "library", Precision: PrecisionPackage},
		{Method: "ALL", Path: "/shelf", Repo: "library", Precision: PrecisionPackage, HandlerUnresolved: true},
		{Method: "ALL", Path: "/", Repo: "archive", Precision: PrecisionPackage, HandlerUnresolved: true},
	})
	var got []string
	for _, e := range out {
		got = append(got, e.Repo+" "+e.Method+" "+e.Path)
	}
	want := []string{
		"library GET /shelves",
		"library ALL /admin",
		"library GET /admin/stats",
		"library ALL /shelf",
		"archive ALL /",
	}
	if !slices.Equal(got, want) {
		t.Errorf("kept %q, want %q", got, want)
	}
}
