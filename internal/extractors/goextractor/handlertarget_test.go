package goextractor

import "testing"

// A handler expression names a function outright when it is a bare identifier (the
// file's own package) or an import alias and a name; a method value or a wrapper
// call names none by itself.
func TestHandlerTarget(t *testing.T) {
	imports := map[string]string{"repo": "routers/web/repo", "uuid": "github.com/google/uuid"}
	for handler, want := range map[string]string{
		"Home":                "routers/web.Home",
		"repo.DownloadPull":   "routers/web/repo.DownloadPull",
		"h.GetUser":           "", // a receiver variable, not an import
		"routing.Wrap(h.Get)": "",
		"a.b.C":               "",
		"":                    "",
	} {
		if got := handlerTarget(handler, "routers/web", imports); got != want {
			t.Errorf("handlerTarget(%q) = %q, want %q", handler, got, want)
		}
	}
}
