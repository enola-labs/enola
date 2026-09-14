// Package vocab holds the tuning knowledge behind cross-repo linking: the word lists
// that decide which names are too generic to link on, the path markers that identify
// non-contract code, and the numeric thresholds the signals compare against.
//
// WHY IT IS A PACKAGE. All of it used to be `var`s and `const`s scattered across 1,800
// lines beside the code that read them. That made two things impossible. It was
// undiscoverable — nobody could see what enola believed about naming without reading
// the linker — and it was unreachable: a user hitting a false edge from a framework
// enola has never seen had no path but a Go patch and a release, even though the fix is
// invariably one more string in one more list.
//
// Nearly every accuracy fix to this linker has been exactly that: adding Rails base
// classes, adding UI-component words, adding generated-code path markers. Those are
// data changes wearing a code change's clothes.
//
// WHY IT IS ONLY DATA. A Set can add vocabulary and move thresholds. It cannot express
// a matching RULE, and deliberately so — a config language that could describe how to
// match would let a user manufacture an edge, and every fact in the graph is supposed
// to be derived rather than asserted. Widening what counts as "too generic to link on"
// can only ever REMOVE edges, which is the safe direction: the linker's governing bias
// is that a missing edge beats a wrong one.
package vocab

import (
	"sort"
	"strconv"
	"strings"
)

// Set is the complete tuning surface. A nil *Set is never valid — use Default().
type Set struct {
	// GenericPathSegments are single-segment endpoint names so widely reused that a
	// path/method match between two repos is no evidence they talk to each other:
	// infra probes, discovery roots, API mount points. Nearly every service serves
	// some of these, so linking on them fabricates edges.
	GenericPathSegments map[string]bool

	// GenericTypeNames are unqualified type names too generic to link on by
	// themselves. Namespaced identities (e.g. "onelab::number") bypass this list —
	// sharing a namespace across repos is itself meaningful.
	GenericTypeNames map[string]bool

	// FrameworkConventions are type identities a framework's own convention or
	// generator makes essentially EVERY app of that framework declare identically, so
	// two apps sharing one is not evidence of shared code — the analog of
	// GenericTypeNames for framework boilerplate rather than generic words. Matched
	// exactly (case-sensitive) and rejected even when namespace-qualified.
	FrameworkConventions map[string]bool

	// GenericComponentNames are the vocabulary of UI-component naming: words so many
	// components are built from that a type named only out of them identifies a role,
	// not a shared implementation. Two frontends each declaring a "SidebarProps" is
	// convention, not shared code — their field sets typically have no overlap at all.
	GenericComponentNames map[string]bool

	// ConventionSuffixes are trailing words a framework's naming convention appends to
	// derive a companion type. They carry no information of their own, so they are
	// stripped before an identity's segments are judged.
	ConventionSuffixes []string

	// NonContractPaths are path fragments identifying files whose declarations are
	// scaffolding rather than portable contract surface: migrations, the
	// test/story/mock/fixture tree, and generated code. Two services consuming the
	// same upstream API each generate identically-named client and model types; that
	// is shared upstream contract, not shared code between the consumers.
	NonContractPaths []string

	// FormatExtensions are response-format suffixes a client may append to a path
	// (Rails renders these as an optional ".:format" that never appears in the route
	// path), stripped before comparing a client call to a server route.
	FormatExtensions []string

	// TopicOwnerSeparator splits a messaging topic name into its owning-service prefix
	// and the rest. By convention a topic is "<owning-service>.<event>", so the
	// leading segment identifies the producer even when that repo cannot be parsed.
	TopicOwnerSeparator string

	Thresholds Thresholds

	// ServiceAliases maps a service name an in-house client passes to the repository
	// label that serves it (config: service_aliases). It is the one entry here that can
	// choose an edge rather than only remove one, and it is bounded for that reason: it
	// applies only to routes read through a declared client, and only chooses among
	// repositories that already serve the called path. Nil when none are declared.
	ServiceAliases map[string]string
}

// Thresholds are the numeric gates. Every one of them trades recall against precision,
// and every default was measured rather than chosen — the doc comments say against what.
type Thresholds struct {
	// MinSharedSegments is the fewest trailing path segments a client and server route
	// must share for a suffix match. Two (e.g. "settings/feedback") keeps the join
	// specific enough to avoid false positives while tolerating the base-path
	// differences between a server's full path and a client's base-relative call.
	MinSharedSegments int

	// MinSharedSymbols is the fewest distinct distinctive type identities two repos
	// must share before a shared-code edge is drawn. Above 2 so an incidental name
	// collision cannot fabricate a dependency, while genuinely vendored code — which
	// shares many — still registers.
	MinSharedSymbols int

	// MaxVocabRepoShare bounds how widely an unqualified identity may be declared
	// before it reads as shared domain vocabulary rather than evidence that any
	// specific pair shares code. Vendored source lands in the handful of repos that
	// copied it; a domain type every service independently models lands in most.
	MaxVocabRepoShare float64

	// MinReposForVocabFilter guards MaxVocabRepoShare so it only applies once enough
	// repos are loaded that "a majority of repos" means something. With two or three,
	// a genuinely vendored header can legitimately appear in most of them.
	MinReposForVocabFilter int

	// MinFileSimilarity is the least token-set overlap the two files declaring a
	// shared type name must have for that name to count as evidence of shared CODE.
	// Measured on real repo pairs the two populations separate cleanly: vendored files
	// score at or near 1.0, same-name-different-code sits far below, so the threshold
	// sits between them rather than near either.
	MinFileSimilarity float64

	// MaxComparedFileBytes skips files too large to be worth comparing line by line; a
	// vendored bundle or generated blob should not dominate link time.
	MaxComparedFileBytes int

	// MaxFactsPerIdentityRepo bounds how many declaring facts are kept per (identity,
	// repo). A class can be reopened across several files; keeping a few lets
	// verification pick the best-matching pair without letting a widely-reopened name
	// explode the comparison count.
	MaxFactsPerIdentityRepo int

	// MaxSamples bounds how many samples an edge fact carries per evidence bucket.
	MaxSamples int
}

// Default returns the built-in vocabulary. It allocates a fresh Set on every call, so a
// caller may overlay onto it without affecting anyone else — the defaults are never
// shared mutable state.
func Default() *Set {
	return &Set{
		GenericPathSegments: set(
			"health", "healthz", "healthcheck",
			"status", "ping", "metrics",
			"ready", "readyz", "live", "livez",
			"version", "info", "api", "graphql",
		),
		GenericTypeNames: set(
			"config", "error", "manager", "options",
			"result", "base", "impl", "utils", "util",
			"common", "exception", "context", "data",
			"info", "item", "node", "entry", "helper",
			"settings", "logger", "test", "main", "model",
			"request", "response", "status", "value",
		),
		// Seeded with Rails (Application* base classes, ActionCable base classes) and
		// CanCanCan (Ability); extend as other frameworks' conventions surface.
		FrameworkConventions: set(
			"ApplicationController", "ApplicationRecord", "ApplicationJob",
			"ApplicationMailer", "ApplicationHelper",
			"ApplicationCable::Connection", "ApplicationCable::Channel",
			"Ability",
		),
		GenericComponentNames: set(
			"footer", "header", "layout", "modal", "card",
			"button", "list", "container", "wrapper",
			"panel", "page", "form", "icon", "badge",
			"avatar", "banner", "dialog", "menu", "nav",
			"sidebar", "tooltip", "spinner", "overlay",
			"toast", "section", "row", "cell", "label",
			"title",
		),
		ConventionSuffixes: []string{"Props", "Types", "Type", "State"},
		NonContractPaths: []string{
			"/db/migrate/",
			"/spec/support/", "/__mocks__/", "/__tests__/", "/fixtures/", "/factories/", "/mocks/",
			".stories.", ".test.", ".spec.", "_test.", "_spec.",
			".gen.", ".pb.", "_pb2.", ".generated.",
		},
		FormatExtensions:    []string{".json", ".xml"},
		TopicOwnerSeparator: ".",
		Thresholds: Thresholds{
			MinSharedSegments:       2,
			MinSharedSymbols:        3,
			MaxVocabRepoShare:       0.5,
			MinReposForVocabFilter:  4,
			MinFileSimilarity:       0.5,
			MaxComparedFileBytes:    1 << 20, // 1 MiB
			MaxFactsPerIdentityRepo: 3,
			MaxSamples:              25,
		},
	}
}

func set(vals ...string) map[string]bool {
	m := make(map[string]bool, len(vals))
	for _, v := range vals {
		m[v] = true
	}
	return m
}

// Fingerprint renders the Set as a stable, sorted string.
//
// It exists because a vocabulary change CHANGES EMITTED FACTS. Folding this into the
// snapshot's config hash is what stops two snapshots taken under different vocabularies
// from comparing as equal — without it, `diff_snapshot` would attribute the resulting
// edge changes to the code and report a delta nobody made.
func (s *Set) Fingerprint() string {
	var sb strings.Builder
	writeSet := func(label string, m map[string]bool) {
		sb.WriteString(label)
		sb.WriteByte(':')
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		sb.WriteString(strings.Join(keys, ","))
		sb.WriteByte('\n')
	}
	writeList := func(label string, vals []string) {
		cp := append([]string(nil), vals...)
		sort.Strings(cp)
		sb.WriteString(label)
		sb.WriteByte(':')
		sb.WriteString(strings.Join(cp, ","))
		sb.WriteByte('\n')
	}
	writeSet("generic_path_segments", s.GenericPathSegments)
	writeSet("generic_type_names", s.GenericTypeNames)
	writeSet("framework_conventions", s.FrameworkConventions)
	writeSet("generic_component_names", s.GenericComponentNames)
	writeList("convention_suffixes", s.ConventionSuffixes)
	writeList("non_contract_paths", s.NonContractPaths)
	writeList("format_extensions", s.FormatExtensions)
	sb.WriteString("topic_owner_separator:" + s.TopicOwnerSeparator + "\n")
	t := s.Thresholds
	sb.WriteString("thresholds:")
	sb.WriteString(join(
		"min_shared_segments", t.MinSharedSegments,
		"min_shared_symbols", t.MinSharedSymbols,
		"min_repos_for_vocab_filter", t.MinReposForVocabFilter,
		"max_compared_file_bytes", t.MaxComparedFileBytes,
		"max_facts_per_identity_repo", t.MaxFactsPerIdentityRepo,
		"max_samples", t.MaxSamples,
	))
	// Floats are rendered at fixed precision so an unchanged value cannot produce a
	// different fingerprint through formatting drift.
	sb.WriteString(fmtFloat("max_vocab_repo_share", t.MaxVocabRepoShare))
	sb.WriteString(fmtFloat("min_file_similarity", t.MinFileSimilarity))
	// Written only when declared, so a vocabulary with no aliases fingerprints exactly as
	// it did before aliases existed.
	if len(s.ServiceAliases) > 0 {
		services := make([]string, 0, len(s.ServiceAliases))
		for k := range s.ServiceAliases {
			services = append(services, k)
		}
		sort.Strings(services)
		sb.WriteString("\nservice_aliases:")
		for _, k := range services {
			sb.WriteString(k + "=" + s.ServiceAliases[k] + ";")
		}
	}
	return sb.String()
}

func join(pairs ...any) string {
	var sb strings.Builder
	for i := 0; i+1 < len(pairs); i += 2 {
		sb.WriteString(pairs[i].(string))
		sb.WriteByte('=')
		sb.WriteString(strconv.Itoa(pairs[i+1].(int)))
		sb.WriteByte(';')
	}
	return sb.String()
}

func fmtFloat(label string, v float64) string {
	return label + "=" + strconv.FormatFloat(v, 'f', 4, 64) + ";"
}
