package kotlinextractor

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
)

// retrofitAnnotation matches a Retrofit HTTP method annotation on an interface
// method, capturing the verb and the path string literal, e.g.
//
//	@GET("/api/settings/entitlements/users/{userID}/active")
//	@POST("auth/login")
//	@GET(value = "topics")
//
// The `value =` form is the same annotation written with its argument named, which
// Kotlin allows for any single-argument annotation and which real Android codebases
// use — Google's own reference app writes every endpoint that way. Matching only the
// positional form yielded ZERO client routes for such a repository, and a Retrofit
// interface with no routes produces no mobile-to-backend edges at all, silently: the
// "which screens break if I change this endpoint" question just returns nothing.
// Found by adding a real Android application to the benchmark corpus.
var retrofitAnnotation = regexp.MustCompile(`@(GET|POST|PUT|DELETE|PATCH|HEAD|OPTIONS)\s*\(\s*(?:value\s*=\s*)?"([^"]*)"`)

// absoluteClientURL matches an absolute http(s) URL in a Retrofit annotation,
// capturing the host and the remaining path. A full URL targets a fixed external
// host, so the call is tagged external (bucketed out of internal coverage) rather
// than matched against a backend route — mirroring the Swift extractor's external
// handling.
var absoluteClientURL = regexp.MustCompile(`^https?://([^/?#]+)(/[^?#]*)?`)

// ioDirectAnnotations are method-level annotations that mark a function as a direct
// network / DB I/O operation: Retrofit HTTP endpoints and Room DAO operations. A
// method carrying one performs a real round-trip, so a per-iteration call to it is a
// genuine N+1 — the annotation is a precise I/O identity, unlike a keyword guess.
var ioDirectAnnotations = map[string]bool{
	// Retrofit / HTTP
	"GET": true, "POST": true, "PUT": true, "DELETE": true,
	"PATCH": true, "HEAD": true, "OPTIONS": true, "HTTP": true,
	// Room DAO operations (@RawQuery/@Transaction run SQL too)
	"Query": true, "Insert": true, "Update": true, "Upsert": true,
	"RawQuery": true,
}

// kotlinIODirectFromAnnotations reports whether a method's annotation simple-names
// include a Retrofit endpoint or Room DAO operation. `@Delete` is matched here too:
// on a DAO method it is a SQL delete. (The names come from annotationNames, which
// yields simple names, so this cannot collide with a same-named type.)
func kotlinIODirectFromAnnotations(annotations []string) bool {
	for _, a := range annotations {
		if ioDirectAnnotations[a] || a == "Delete" {
			return true
		}
	}
	return false
}

// extractRetrofitFacts emits a client-route fact for every Retrofit endpoint
// annotation in a Kotlin source file. These represent outbound HTTP calls the
// app makes, so the cross-repo linker can match them (by method + path suffix)
// to the backend route that serves them. Path prefixes are inconsistent across
// services (some "/api/...", some base-relative) — suffix matching reconciles
// that, so we emit the path as written.
func extractRetrofitFacts(src []byte, relFile string) []facts.Fact {
	var out []facts.Fact
	dir := factpath.Dir(relFile)
	api := apiHint(relFile)
	consts := fileStringConsts(src)

	for i, line := range strings.Split(string(src), "\n") {
		m := retrofitAnnotation.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		method := strings.ToUpper(m[1])
		path := cleanClientPath(substituteConsts(m[2], consts))
		if path == "" {
			continue
		}
		props := map[string]any{
			facts.PropRole:   facts.RoleClient,
			"method":         method,
			"framework":      "retrofit",
			"language":       "kotlin",
			facts.PropSource: facts.RouteSourceRetrofit,
			"api":            api,
		}
		// A full http(s):// URL targets a fixed external host: tag it external + host
		// and reduce the Name to the base-relative path so it reads consistently.
		if hm := absoluteClientURL.FindStringSubmatch(path); hm != nil {
			props["external"] = true
			props["host"] = hm[1]
			if rest := strings.Trim(hm[2], "/"); rest != "" {
				path = rest
			} else {
				path = hm[1]
			}
		}
		out = append(out, facts.Fact{
			Kind:      facts.KindRoute,
			Name:      path,
			File:      relFile,
			Line:      i + 1,
			Props:     props,
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: dir}},
		})
	}
	return out
}

// stringConst matches a Kotlin string constant with a plain literal value:
// `const val ROUTE_PREFIX = "orders/api/v2"`, top level or in a companion.
var stringConst = regexp.MustCompile(`\bconst\s+val\s+([A-Za-z_][A-Za-z0-9_]*)\s*(?::\s*String\s*)?=\s*"([^"$\\]*)"`)

// stringTemplateRef matches a template reference in a string literal: `${NAME}`
// or `$NAME`.
var stringTemplateRef = regexp.MustCompile(`\$\{\s*([A-Za-z_][A-Za-z0-9_]*)\s*\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// fileStringConsts returns the string constants a file declares. A Retrofit
// interface commonly keeps the service prefix in its companion and writes
// "${ROUTE_PREFIX}/read" in every annotation.
func fileStringConsts(src []byte) map[string]string {
	ms := stringConst.FindAllSubmatch(src, -1)
	if len(ms) == 0 {
		return nil
	}
	out := make(map[string]string, len(ms))
	for _, m := range ms {
		out[string(m[1])] = string(m[2])
	}
	return out
}

// substituteConsts replaces the template references to a file's string constants
// in a path. A reference to anything else is left as written.
func substituteConsts(path string, consts map[string]string) string {
	if len(consts) == 0 || !strings.Contains(path, "$") {
		return path
	}
	return stringTemplateRef.ReplaceAllStringFunc(path, func(ref string) string {
		m := stringTemplateRef.FindStringSubmatch(ref)
		name := m[1]
		if name == "" {
			name = m[2]
		}
		if v, ok := consts[name]; ok {
			return v
		}
		return ref
	})
}

// cleanClientPath strips a query string from a client route path and trims
// surrounding whitespace. Path parameters are left as written; the linker's
// normalizePath collapses {x}/:x/<x> forms.
func cleanClientPath(p string) string {
	p = strings.TrimSpace(p)
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	return p
}

// apiHint returns the source file's base name without extension (e.g.
// "EntitlementApiService"), used as the cross-repo linker's provider
// disambiguation hint.
func apiHint(relFile string) string {
	base := filepath.Base(relFile)
	return strings.TrimSuffix(base, filepath.Ext(base))
}
