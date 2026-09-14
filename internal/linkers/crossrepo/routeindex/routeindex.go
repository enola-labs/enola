// Package routeindex holds the route/path primitives the cross-repo HTTP signal is
// built from: path normalization, the server-route suffix index, and the matching
// rules that decide whether a client call and a server route are the same endpoint.
//
// It is its own package because three passes have to agree on those rules EXACTLY —
// the HTTP signal that draws the edge, and the two inverse passes that report what
// went unmatched. When they lived beside each other as unexported helpers, "must stay
// in lockstep with linkHTTP" appeared three separate times as a comment. Here it is a
// type, and the compiler enforces what the comment asked for.
package routeindex

import (
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/linkers/vocab"
)

// Matcher applies the route-matching rules under one vocabulary. The rules that consult
// tunable knowledge — which single-segment paths are too generic to link on, which
// response-format suffixes to strip, how many trailing segments a suffix match needs —
// are methods on it rather than package functions, so a snapshot's vocabulary reaches
// them explicitly instead of through a package-level global that tests would have to
// mutate and concurrent snapshots would share.
//
// Rules that consult nothing tunable (RouteIdentity, NormalizeMethod, RoleOf, …) stay
// package-level: making them methods would imply a configurability they do not have.
type Matcher struct {
	v *vocab.Set
}

// New returns a Matcher using the given vocabulary.
func New(v *vocab.Set) *Matcher { return &Matcher{v: v} }

// ServiceAlias returns the repository label the config's service_aliases maps a service
// name to. Keys match exactly: the name is the string the client passes, and whoever
// writes the alias writes that same string.
func (m *Matcher) ServiceAlias(service string) (string, bool) {
	if service == "" {
		return "", false
	}
	repo := m.v.ServiceAliases[service]
	return repo, repo != ""
}

type RouteRef struct {
	Repo     string
	Method   string
	Path     string
	FullPath string // the complete normalized server path this ref was indexed from
}

// IndexServerRoutes builds the suffix+method -> server route index the HTTP linker
// matches client calls against: every trailing-segment suffix of every server
// route's normalized path(s), keyed by RouteKey. Shared by linkHTTP and
// UnmatchedClientRouteKeys so both resolve a client call identically. ServerPaths
// already returns normalized paths; fullPath records which full path a suffix came
// from, so a match can tell a full-path hit from a fragment hit.
func (m *Matcher) IndexServerRoutes(all []facts.Fact) map[string][]RouteRef {
	server := map[string][]RouteRef{}
	for _, f := range all {
		if f.Kind != facts.KindRoute || f.Repo == "" || RoleOf(f) == facts.RoleClient || IsUIRoute(f) ||
			f.PropString(facts.PropRouteType) == facts.RouteTypeGraphQL {
			continue
		}
		method := NormalizeMethod(f.PropString("method"))
		if method == "" {
			continue
		}
		for _, p := range m.ServerPaths(f) {
			ref := RouteRef{Repo: f.Repo, Method: method, Path: f.Name, FullPath: p}
			for _, suf := range m.PathMatchKeys(p) {
				server[RouteKey(suf, method)] = append(server[RouteKey(suf, method)], ref)
			}
		}
	}
	return server
}

// IndexServerPathSuffixes returns the set of server route path-suffixes ignoring
// method — the method-agnostic counterpart to IndexServerRoutes. It lets the
// unmatched-client pass tell "the endpoint exists but we have the wrong verb"
// (method_mismatch) from "no server route serves this path at all" (path_unknown).
func (m *Matcher) IndexServerPathSuffixes(all []facts.Fact) map[string]bool {
	set := map[string]bool{}
	for _, f := range all {
		if f.Kind != facts.KindRoute || f.Repo == "" || RoleOf(f) == facts.RoleClient {
			continue
		}
		if NormalizeMethod(f.PropString("method")) == "" {
			continue
		}
		for _, p := range m.ServerPaths(f) {
			for _, suf := range m.PathMatchKeys(p) {
				set[suf] = true
			}
		}
	}
	return set
}

// ServerPaths returns the normalized paths a server route is reachable at: its
// own path and, when present, its gateway path (prefix + path).
func (m *Matcher) ServerPaths(f facts.Fact) []string {
	out := []string{m.NormalizePath(f.Name)}
	if gw := f.PropString("gateway_path"); gw != "" {
		out = append(out, m.NormalizePath(gw))
	}
	return out
}

// RouteIdentity returns the stable identity key of a route fact — its repo, HTTP
// method (normalized the same way the matcher normalizes), and path (the fact
// Name). The keys in the set returned by UnmatchedServerRouteKeys use this exact
// form, so a caller can flag the matching route facts without re-deriving any
// path or method normalization of its own.
func RouteIdentity(f facts.Fact) string {
	return RouteIdentityKey(f.Repo, NormalizeMethod(f.PropString("method")), f.Name)
}

func RouteIdentityKey(repo, method, path string) string {
	return repo + "\x00" + method + "\x00" + path
}

// UnmatchedServerRouteKeys returns the identities (see RouteIdentity) of server
// routes that no loaded client route resolves to — endpoints unused by every
// client in the snapshot. It reuses the cross-repo HTTP linker's exact server
// index and suffix/method matching, so a route flagged here is precisely one the
// linker found no caller for; only the verdict differs from linkHTTP.
//
// Matching is deliberately generous: a server route counts as used on any
// suffix+method hit, regardless of match confidence or which repo the caller is
// in. This biases the unused set toward false negatives — it will never flag a
// route that shows any sign of use, which is the safe direction when the output
// may drive endpoint removal. Returns nil for single-repo snapshots: with no

// ClientPathHasServer reports whether any of a client path's trailing-segment
// suffixes matches a server route path suffix regardless of verb (mirrors
// LookupClientMatches' key generation, minus the method filter — including its
// whole-path fallback, so a single-segment client path is tested against the
// index rather than reported path_unknown without ever being looked up).
func (m *Matcher) ClientPathHasServer(serverSuffixes map[string]bool, clientPath string) bool {
	for _, suf := range m.PathMatchKeys(clientPath) {
		if serverSuffixes[suf] {
			return true
		}
	}
	return false
}

// RepoFromIdentity extracts the repo label from a route identity key (see
// routeIdentityKey, which prefixes the repo before the first NUL separator).
func RepoFromIdentity(id string) string {
	if i := strings.IndexByte(id, '\x00'); i >= 0 {
		return id[:i]
	}
	return id
}

// IsLinkable reports whether a server route is one this matcher can reason about
// at all: it must be a repo-labelled server route with an HTTP verb, and not a UI
// page route, a GraphQL operation or a generic path the matcher refuses to link on.
//
// It exists as a predicate rather than as an inline chain because the answer has
// to be reached per FACT and not per identity. RouteIdentity is repo + method +
// path, and an Ember page route sitting on the same path as a Rails route shares
// its key: 54 identities in the estate cover 119 route facts, 29 of them mixing a
// page route with a served one. A caller that decides "did this route match?" by
// looking its identity up in a set built from linkable routes hands the Rails
// route's verdict to the page route beside it — which is how `/*path` came to be
// reported as an unused endpoint despite IsUIRoute existing to prevent exactly
// that.
func (m *Matcher) IsLinkable(f facts.Fact) bool {
	if f.Kind != facts.KindRoute || f.Repo == "" || RoleOf(f) == facts.RoleClient ||
		IsUIRoute(f) || f.PropString(facts.PropRouteType) == facts.RouteTypeGraphQL {
		return false
	}
	if NormalizeMethod(f.PropString("method")) == "" {
		return false
	}
	return !m.IsGenericPath(m.NormalizePath(f.Name))
}

func RoleOf(f facts.Fact) string { return f.PropString(facts.PropRole) }

// uiRouteTypes are the `type` values the file-based/DSL UI routers stamp on
// page-navigation routes (Next.js pages, Nuxt pages, SvelteKit +page/+layout/
// +error, Ember's router map, Vue router-config files). A UI route is a URL the
// BROWSER navigates to, not an HTTP contract this repo serves to other services
// — indexing one as a server route makes every page a candidate cross-repo
// match and, worse, an "unused route no client calls" finding. Next.js App
// Router API handlers carry type "route" and are deliberately NOT here.
//
// "loading" is a Suspense fallback component, as purely presentational as the
// "error" boundary beside it, and its omission was not harmless: a loading.tsx
// under a single dynamic segment (app/[slug]/loading.tsx) extracts as the server
// route "/{}", which matches ANY one-segment client call with a path parameter —
// enough for a frontend to collect inbound dependencies off that one route. The
// set must therefore track every convention basename the Next.js branch of the TS
// extractor emits (see tsextractor/ts.go, the App Router basename check) except
// "route".
var uiRouteTypes = map[string]bool{
	"page": true, "layout": true, "error": true, "loading": true,
	"router_config": true, "engine_mount": true,
}

// IsUIRoute reports whether a route fact is a client-side navigation route
// rather than a served HTTP endpoint.
func IsUIRoute(f facts.Fact) bool { return uiRouteTypes[f.PropString("type")] }

// IsExternalClient reports whether a client route targets a hardcoded external host
// (tagged external=true by the extractor). Tolerates the bool surviving a JSON
// round-trip as a bool literal.
func IsExternalClient(f facts.Fact) bool {
	if f.Props == nil {
		return false
	}
	v, _ := f.Props["external"].(bool)
	return v
}

func NormalizeMethod(m string) string {
	m = strings.ToUpper(strings.TrimSpace(m))
	switch m {
	case "", "USE", "ALL", "MIDDLEWARE", "DRAW":
		// DRAW is a Rails route-delegation placeholder (config/routes.rb draw(:pkg)),
		// not a real HTTP method — inert for matching and unused-route flagging.
		return ""
	}
	return m
}

// RouteKey is the index key for one (path-suffix, method) pair. Exported because the
// unused-route pass builds its own index over the same keyspace.
func RouteKey(normPath, method string) string { return normPath + "|" + method }

// stripFormatExtension removes a trailing response-format extension from a single
// path segment: "orders.json" -> "orders", "{id}.json" -> "{id}" (so the later
// {}-collapse still fires). Only the known format suffixes are stripped, so a
// version-like segment ("v2.5") or a real dotted name is left intact.
func (m *Matcher) stripFormatExtension(seg string) string {
	for _, ext := range m.v.FormatExtensions {
		if len(seg) > len(ext) && strings.HasSuffix(seg, ext) {
			return seg[:len(seg)-len(ext)]
		}
	}
	return seg
}

// NormalizePath trims a trailing slash, strips a trailing response-format extension
// (.json/.xml) from the final segment, and collapses path parameters in any
// framework syntax ({id}, :id, <id>) to a single "{}" placeholder, so a client
// path matches the server path it calls regardless of param naming or format suffix.
func (m *Matcher) NormalizePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if len(p) > 1 {
		p = strings.TrimRight(p, "/")
	}
	segs := strings.Split(p, "/")
	// Strip a format extension from the final segment before param collapse, so a
	// client call "/devices/{id}.json" normalizes to the same "/devices/{}" the
	// server route "/devices/:id" produces.
	if n := len(segs); n > 0 {
		segs[n-1] = m.stripFormatExtension(segs[n-1])
	}
	for i, s := range segs {
		switch {
		case strings.HasPrefix(s, ":"):
			segs[i] = "{}"
		case strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}"):
			segs[i] = "{}"
		case strings.HasPrefix(s, "<") && strings.HasSuffix(s, ">"):
			segs[i] = "{}"
		case strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]"):
			// Next.js dynamic segments: [id], [slug], [...rest].
			segs[i] = "{}"
		}
	}
	return strings.Join(segs, "/")
}

// pathSuffixes returns every trailing-segment suffix of a normalized path that
// has at least m.v.Thresholds.MinSharedSegments non-empty segments, longest first. Each suffix
// is rendered leading-slash-canonical ("/seg/seg/..."), so a server path
// "/api/settings/entitlements/definitions" yields:
//
//	/api/settings/entitlements/definitions
//	/settings/entitlements/definitions
//	/entitlements/definitions
//
// ("definitions" alone is dropped: below m.v.Thresholds.MinSharedSegments). This lets a client
// calling a base-relative subpath match the server serving the full path.
func (m *Matcher) pathSuffixes(normPath string) []string {
	var segs []string
	for _, s := range strings.Split(normPath, "/") {
		if s != "" {
			segs = append(segs, s)
		}
	}
	var out []string
	for start := 0; start+m.v.Thresholds.MinSharedSegments <= len(segs); start++ {
		out = append(out, "/"+strings.Join(segs[start:], "/"))
	}
	return out
}

// PathMatchKeys returns the path forms to join a route on — its suffixes, or the
// whole path when it is too short to have any.
//
// pathSuffixes yields nothing below m.v.Thresholds.MinSharedSegments, because a lone trailing
// segment is too weak to join a PREFIXED path on. But used bare it also drops
// single-segment routes from the join entirely: a server serving GET /widgets
// was indexed under no key at all, so no client could ever match it, and the
// client side reported path_unknown against an index the path had never been
// admitted to. LookupClientMatches already worked around this on one side with a
// whole-path fallback; centralizing it here makes every builder and lookup agree.
//
// Only the exact full path is ever added, never a bare trailing segment of a
// longer path, so this admits exact single-segment matches without weakening
// suffix matching. Fabrication is held off on the client side instead, where
// IsGenericPath drops infra names (/health, /status) and linkHTTP demands an
// unambiguous provider before drawing a one-segment edge.
func (m *Matcher) PathMatchKeys(normPath string) []string {
	if sufs := m.pathSuffixes(normPath); len(sufs) > 0 {
		return sufs
	}
	return []string{normPath}
}

// LookupClientMatches resolves a client path against the server suffix index by
// trying the client path's own trailing-segment suffixes longest-first and
// returning the first (most specific) hit, plus the suffix that matched. The
// longest suffix is the full client path, so an exact full-path match still wins
// first; shorter suffixes then let a prefixed client path ("/api/settings/x/y")
// match a server serving the un-prefixed path ("/x/y"). For sub-m.v.Thresholds.MinSharedSegments
// paths (no suffixes) it falls back to a single full-path lookup, preserving the
// original behavior.
func (m *Matcher) LookupClientMatches(server map[string][]RouteRef, clientPath, method string) ([]RouteRef, string) {
	sufs := m.pathSuffixes(clientPath)
	if len(sufs) == 0 {
		if m := server[RouteKey(clientPath, method)]; len(m) > 0 {
			return m, clientPath
		}
		if m := server[RouteKey(clientPath, facts.MethodAny)]; len(m) > 0 {
			return m, clientPath
		}
		if method == facts.MethodAny {
			for _, v := range clientAnyVerbs {
				if m := server[RouteKey(clientPath, v)]; len(m) > 0 {
					return m, clientPath
				}
			}
		}
		return nil, clientPath
	}
	for _, suf := range sufs {
		// Prefer an exact-verb server route at this suffix, then fall back to a
		// wildcard route (facts.MethodAny — a servlet or verb-less mapping that serves
		// every method), so a concrete client call still resolves to it.
		if m := server[RouteKey(suf, method)]; len(m) > 0 {
			return m, suf
		}
		if m := server[RouteKey(suf, facts.MethodAny)]; len(m) > 0 {
			return m, suf
		}
		// The symmetric wildcard: a CLIENT whose method the source does not state
		// (facts.MethodAny — an options-object call with no verb property, where a
		// protocol library chooses the verb at runtime) matches the path's server
		// whichever verb serves it, in a fixed preference order so the choice is
		// deterministic.
		if method == facts.MethodAny {
			for _, v := range clientAnyVerbs {
				if m := server[RouteKey(suf, v)]; len(m) > 0 {
					return m, suf
				}
			}
		}
	}
	return nil, clientPath
}

// clientAnyVerbs is the deterministic verb-preference order a method-less
// client call tries against a suffix's server routes.
var clientAnyVerbs = []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD"}

// CanonicalLeadingSlash ensures a non-empty path starts with "/", so a
// base-relative client path ("settings/x") compares equal to the indexed
// suffix form ("/settings/x").
func CanonicalLeadingSlash(p string) string {
	if p == "" || strings.HasPrefix(p, "/") {
		return p
	}
	return "/" + p
}

// IsGenericPath reports whether a path is too low-signal to safely link on.
//
// This was originally a pure segment count (fewer than two segments = generic),
// which swept up every legitimate single-segment endpoint — a client POSTing to
// /activate could never resolve to the service serving POST /activate, and the
// missed edge surfaced only as an unresolved coverage gap. The count is a proxy
// for the real property (is this name so common that a match proves nothing?),
// so test that property directly: a lone segment is generic when it is one of
// the well-known infra/mount names above, not merely because it is alone.
//
// A single-segment path that clears the vocabulary is still weaker evidence
// than a multi-segment one, so linkHTTP additionally requires its match to be
// unambiguous — see the SingleSegmentPath gate there.
func (m *Matcher) IsGenericPath(normPath string) bool {
	var segs []string
	for _, s := range strings.Split(normPath, "/") {
		if s != "" {
			segs = append(segs, s)
		}
	}
	if strings.Contains(normPath, "{}") {
		return false
	}
	if len(segs) == 0 {
		return true // bare "/"
	}
	if len(segs) == 1 {
		return m.v.GenericPathSegments[strings.ToLower(segs[0])]
	}
	return false
}

// SingleSegmentPath reports whether a normalized path has exactly one segment
// and no path parameter. Such a path passes IsGenericPath only by clearing the
// generic vocabulary; linkHTTP still demands an unambiguous provider before
// drawing an edge from one, keeping the linker's preference for a missing edge
// over a fabricated one.
func SingleSegmentPath(normPath string) bool {
	if strings.Contains(normPath, "{}") {
		return false
	}
	n := 0
	for _, s := range strings.Split(normPath, "/") {
		if s != "" {
			n++
		}
	}
	return n == 1
}

// normalizeLabel lowercases and strips '-'/'_' so "app-web",
// "app_web", and "AppWeb" all compare equal.
