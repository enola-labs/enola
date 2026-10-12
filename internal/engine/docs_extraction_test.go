package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The docs/extraction/*.md pages describe what each extractor produces, using
// committed fixtures as the evidence. Prose has no test suite, so an extractor can
// change while the page keeps describing an outcome that stopped happening — the
// same failure mode `TestPublishedExample_StillDemonstratesWhatItClaims` exists to
// prevent for examples/cross-repo.
//
// These tests assert the two things that would rot silently:
//   1. every fixture and golden the pages point at still exists;
//   2. the specific composed paths the pages present as the headline capability
//      are still what the golden files contain.
//
// A page that stops being true fails here before anyone reads it.

const docsExtractionDir = "../../docs/extraction"

func goldenRouteNames(t *testing.T, fixture string) map[string]bool {
	t.Helper()
	path := filepath.Join("testdata", "golden", fixture+".facts.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden %s: %v", path, err)
	}
	names := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var f struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		if f.Kind == "route" {
			names[f.Name] = true
		}
	}
	return names
}

// TestExtractionDocs_ReferencedFixturesExist keeps the pages' links honest: every
// testdata path they name has to be a real directory or file.
func TestExtractionDocs_ReferencedFixturesExist(t *testing.T) {
	entries, err := os.ReadDir(docsExtractionDir)
	if err != nil {
		t.Skipf("docs/extraction not present: %v", err)
	}
	// Matches the repo-relative testdata paths the pages link to, e.g.
	// ../../internal/engine/testdata/repos/go_sample/
	ref := regexp.MustCompile(`internal/engine/testdata/(repos|golden)/([A-Za-z0-9_.-]+)`)

	var checked int
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(docsExtractionDir, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		for _, m := range ref.FindAllStringSubmatch(string(body), -1) {
			target := filepath.Join("testdata", m[1], m[2])
			if _, err := os.Stat(target); err != nil {
				t.Errorf("%s references %s, which does not exist", e.Name(), m[0])
			}
			checked++
		}
	}
	if checked == 0 {
		t.Error("no fixture references found in docs/extraction — the regex or the pages changed")
	}
}

// TestExtractionDocs_ComposedPathsStillHold pins the claim each page is built
// around: a route registered at a bare path is STORED at its composed runtime
// path. If prefix composition regresses, these fail with the page still promising
// it works.
func TestExtractionDocs_ComposedPathsStillHold(t *testing.T) {
	cases := []struct {
		fixture string
		page    string
		want    []string
		absent  []string // paths that must NOT survive composition
	}{
		{
			fixture: "go_httpclient_multirepo",
			page:    "go.md",
			// registered as "/things/{id}" inside registerThings, mounted at "/v1" in main
			want:   []string{"/v1/things/{id}"},
			absent: []string{"/things/{id}"},
		},
		{
			fixture: "py_fastapi_multirepo",
			page:    "python.md",
			// declared as "/results" and "/" in a router factory, mounted at "/api/v1/search"
			want:   []string{"/api/v1/search/results", "/api/v1/search"},
			absent: []string{"/results"},
		},
		{
			fixture: "ts_express_multirepo",
			page:    "typescript.md",
			// registered as "/login" in routes/webhooks.js, mounted at "/webhooks" in index.js
			want:   []string{"/webhooks/login"},
			absent: []string{"/login"},
		},
		{
			fixture: "ts_nest_multirepo",
			page:    "typescript.md",
			// @Get('available') under @Controller('v2/slots')
			want:   []string{"/v2/slots/available", "/v2/slots/reserve"},
			absent: []string{"/available", "/reserve"},
		},
		{
			fixture: "ruby_sample",
			page:    "ruby.md",
			// a singular `resource` nested in a plural `resources` has no :id of its own
			want: []string{
				"/admin/reports/:report_id/export",
				"/admin/reports/:report_id/sections/:id",
			},
		},
		{
			fixture: "scala_sample",
			page:    "scala.md",
			// The page presents three composed shapes as the headline capability:
			// a Play include mounted at its prefix, a Pekko pathPrefix composed into
			// path, and an http4s extractor variable normalized to :id.
			want: []string{
				"/admin/users", "/admin/users/:id",
				"/api/state", "/api/disable", "/api/v2/admin/ping",
				"/users/:id", "/teams",
			},
			// And two it states are composed AWAY: the doubled mount prefix, and the
			// bare sub-router path that would exist if the include were ignored.
			absent: []string{"/team/team", "/state"},
		},
		{
			fixture: "php_laravel_sample",
			page:    "php.md",
			// Route::resource('photos', …) expands to one route per action, each
			// record addressed by the singular of the resource
			want: []string{
				"/api/photos", "/api/photos/create", "/api/photos/{photo}", "/api/photos/{photo}/edit",
				// routes/internal.php declares "/profiles/{id}"; bootstrap/app.php
				// loads it under the "internal" prefix
				"/internal/profiles/{id}",
			},
			absent: []string{"/api/photos/{id}", "/profiles/{id}"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			routes := goldenRouteNames(t, tc.fixture)
			for _, w := range tc.want {
				if !routes[w] {
					t.Errorf("docs/extraction/%s presents %q as a composed route, but %s's golden has no such route",
						tc.page, w, tc.fixture)
				}
			}
			for _, a := range tc.absent {
				if routes[a] {
					t.Errorf("docs/extraction/%s states %q is composed away, but %s's golden still contains it",
						tc.page, a, tc.fixture)
				}
			}
		})
	}
}
