package rubyextractor

import (
	"log"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/gqlscan"
)

// graphql-ruby contract extraction — the server half of the GraphQL seam.
//
// A graphql-ruby schema's operation surface is declared with the `field` DSL
// inside the root types (QueryType / MutationType / SubscriptionType). Those
// declarations are the contract a client's operation names bind to, so each
// becomes a route fact (`Query.fieldName`, type=graphql, role=server) — the
// composed-path idea from NestJS applied to GraphQL's root fields. Non-root
// types' fields are schema internals, not operations, and emit nothing.

// A root type is a class, or a module prepended to one (`module EE; module Types;
// module MutationType; prepended do … end`, how an edition adds root fields).
var graphqlRootClass = regexp.MustCompile(`(?:class|module)\s+(?:[A-Z]\w*::)*(Query|Mutation|Subscription)Type\b`)

// A mutation mounted by class: graphql-ruby's mount_mutation names the field after
// the class's graphql_name, which is declared in the mutation's own file, so the
// route is a placeholder until bindGraphQLMounts reads it. An aliased mount states
// its name.
var (
	graphqlMountDecl        = regexp.MustCompile(`(?m)^\s*mount_mutation\s+((?:::)?[A-Z][\w:]*)`)
	graphqlAliasedMountDecl = regexp.MustCompile(`(?m)^\s*mount_aliased_mutation\s+['"](\w+)['"]\s*,\s*((?:::)?[A-Z][\w:]*)`)
	rubyNamespaceDecl       = regexp.MustCompile(`(?m)^\s*(?:module|class)\s+([A-Z][\w:]*)`)
)

// Props a mounted mutation's route carries until bindGraphQLMounts settles it.
const (
	propGraphQLMountClass = "graphql_mount_class"
	propGraphQLMountScope = "graphql_mount_scope"
	propGraphQLName       = "graphql_name"
)

// graphqlFieldDecl matches `field :name` at a plausible DSL position. The
// symbol form is the graphql-ruby convention; a string first argument is not
// part of the root-field DSL.
var graphqlFieldDecl = regexp.MustCompile(`(?m)^\s*field\s+:([a-z_][a-z0-9_]*)`)

// extractGraphQLRubyRoutes emits one server-role graphql route per root field.
// Field names camelize exactly as graphql-ruby does by default (snake_case →
// camelCase); a schema overriding that default would need configuration this
// deliberately does not read — the miss is visible as an unmatched client op.
func extractGraphQLRubyRoutes(src []byte, relFile string) []facts.Fact {
	if !strings.Contains(filepath.ToSlash(relFile), "graphql") {
		return nil
	}
	root := graphqlRootClass.FindSubmatchIndex(src)
	if root == nil {
		// A module of mounts included into the mutation type (`module Types;
		// module DeprecatedMutations; included do mount_aliased_mutation …`): a mount
		// only means anything on the Mutation root, so its mounts are read; its other
		// fields are not, since the root they land on is not stated.
		first := graphqlMountDecl.FindIndex(src)
		if a := graphqlAliasedMountDecl.FindIndex(src); a != nil && (first == nil || a[0] < first[0]) {
			first = a
		}
		if first == nil {
			return nil
		}
		return graphqlMountRoutes(src, relFile, first[0])
	}
	kind := string(src[root[2]:root[3]])
	var out []facts.Fact
	seen := map[string]bool{}
	if kind == "Mutation" {
		out = append(out, graphqlMountRoutes(src, relFile, root[0])...)
	}
	for _, m := range graphqlFieldDecl.FindAllSubmatchIndex(src, -1) {
		name := graphqlCamelize(string(src[m[2]:m[3]]))
		full := kind + "." + name
		if seen[full] {
			continue
		}
		seen[full] = true
		out = append(out, facts.Fact{
			Kind: facts.KindRoute,
			Name: full,
			File: relFile,
			Line: 1 + countNewlines(src[:m[0]]),
			Props: map[string]any{
				"language":          "ruby",
				"framework":         "graphql-ruby",
				facts.PropRole:      facts.RoleServer,
				facts.PropRouteType: facts.RouteTypeGraphQL,
				facts.PropSource:    facts.RouteSourceGraphQLRubyDSL,
			},
		})
	}
	return out
}

// Ruby GraphQL CLIENT operations — the other half of the seam a Rails service
// calls when its GraphQL API lives in a sibling service. The operation
// document is a plain Ruby string (a quoted literal or a heredoc body), so
// extraction anchors on positions where a string literal begins: a quote
// character immediately followed by an operation head, or a heredoc opener
// whose body opens with one. A bare `query { … }` in Ruby code position never
// matches — that is Ruby's own block syntax, not GraphQL.

// gqlQuotedHead anchors on an OPENING quote: the keyword must follow the
// quote on the same line (a closing quote at end-of-line trailed by Ruby code
// like `query.where(…) … {` spans newlines and matched a looser pattern), and
// what follows must be structurally an operation head — optional name,
// optional parenthesized variables, opening brace — so a method call between
// keyword and brace can never qualify.
var (
	gqlQuotedHead  = regexp.MustCompile(`["'][ \t]*(query|mutation|subscription)\b[ \t]*(?:[A-Za-z_]\w*)?[ \t]*(?:\([^)]*\))?\s*\{`)
	gqlHeredocOpen = regexp.MustCompile(`<<[~-]?["']?([A-Z_][A-Z0-9_]*)["']?`)
)

// extractGraphQLRubyClientOps emits one client-role graphql route per root
// field of every operation literal in the file. Server-side schema files are
// excluded twice over: anything under a graphql/ directory (the graphql-ruby
// convention tree, whose description strings may quote example operations)
// and any file declaring a root type class.
func extractGraphQLRubyClientOps(src []byte, relFile string) []facts.Fact {
	slashed := filepath.ToSlash(relFile)
	if strings.Contains(slashed, "graphql/") || facts.IsTestPath(relFile) {
		return nil
	}
	text := string(src)
	if !strings.Contains(text, "query") && !strings.Contains(text, "mutation") &&
		!strings.Contains(text, "subscription") {
		return nil
	}
	if graphqlRootClass.MatchString(text) {
		return nil
	}
	var out []facts.Fact
	seen := map[string]bool{}
	emit := func(kind, body string, offset int) {
		kindName := strings.ToUpper(kind[:1]) + kind[1:]
		for _, field := range gqlscan.RootFields(body) {
			full := kindName + "." + field
			if seen[full] {
				continue
			}
			seen[full] = true
			out = append(out, facts.Fact{
				Kind: facts.KindRoute,
				Name: full,
				File: relFile,
				Line: 1 + countNewlines(src[:offset]),
				Props: map[string]any{
					"language":          "ruby",
					"framework":         "graphql",
					facts.PropRole:      facts.RoleClient,
					facts.PropRouteType: facts.RouteTypeGraphQL,
					facts.PropSource:    facts.RouteSourceGraphQLRubyString,
				},
			})
		}
	}
	for _, m := range gqlQuotedHead.FindAllStringSubmatchIndex(text, -1) {
		emit(text[m[2]:m[3]], text[m[1]:], m[0])
	}
	for _, hm := range gqlHeredocOpen.FindAllStringSubmatchIndex(text, -1) {
		terminator := text[hm[2]:hm[3]]
		bodyStart := strings.IndexByte(text[hm[1]:], '\n')
		if bodyStart < 0 {
			continue
		}
		bodyStart += hm[1] + 1
		end := regexp.MustCompile(`(?m)^\s*` + terminator + `\s*$`).FindStringIndex(text[bodyStart:])
		if end == nil {
			continue
		}
		body := text[bodyStart : bodyStart+end[0]]
		for _, om := range gqlscan.OperationHeads(body) {
			emit(body[om[2]:om[3]], body[om[1]:], bodyStart+om[0])
		}
	}
	return out
}

// graphqlMountRoutes reads a mutation root's mount_mutation and
// mount_aliased_mutation declarations.
func graphqlMountRoutes(src []byte, relFile string, rootAt int) []facts.Fact {
	// The lexical scope the mounted constant is resolved in: the modules and classes
	// opened down to the root type, one per line, as a root type file is written.
	var scope []string
	for _, m := range rubyNamespaceDecl.FindAllSubmatchIndex(src, -1) {
		if m[0] > rootAt {
			break
		}
		scope = append(scope, string(src[m[2]:m[3]]))
	}
	props := func() map[string]any {
		return map[string]any{
			"language": "ruby", "framework": "graphql-ruby",
			facts.PropRole: facts.RoleServer, facts.PropRouteType: facts.RouteTypeGraphQL,
			facts.PropSource: facts.RouteSourceGraphQLRubyDSL,
		}
	}
	var out []facts.Fact
	for _, m := range graphqlMountDecl.FindAllSubmatchIndex(src, -1) {
		class := string(src[m[2]:m[3]])
		p := props()
		p[propGraphQLMountClass], p[propGraphQLMountScope] = class, strings.Join(scope, "::")
		out = append(out, facts.Fact{Kind: facts.KindRoute, Name: "Mutation." + class, File: relFile, Line: 1 + countNewlines(src[:m[0]]), Props: p})
	}
	for _, m := range graphqlAliasedMountDecl.FindAllSubmatchIndex(src, -1) {
		p := props()
		p[propGraphQLMountClass], p[propGraphQLMountScope] = string(src[m[4]:m[5]]), strings.Join(scope, "::")
		p[propGraphQLName] = string(src[m[2]:m[3]])
		out = append(out, facts.Fact{Kind: facts.KindRoute, Name: "Mutation." + string(src[m[2]:m[3]]), File: relFile, Line: 1 + countNewlines(src[:m[0]]), Props: p})
	}
	return out
}

// bindGraphQLMounts names each mounted mutation's field after its class's
// graphql_name, as graphql-ruby does (`graphql_name.underscore`, camelized), and
// binds it handled_by the class's resolve. A mount whose class or graphql_name
// cannot be read is dropped rather than named after a guess: graphql-ruby's
// default would be the bare class name, which the class rarely keeps.
func bindGraphQLMounts(all []facts.Fact) []facts.Fact {
	ix := buildHandlerIndex(all)
	names := map[string]string{}
	for _, f := range all {
		if f.Kind == facts.KindSymbol {
			if n := f.PropString(propGraphQLName); n != "" {
				names[f.Name] = n
			}
		}
	}
	kept := all[:0]
	dropped := 0
	for _, f := range all {
		class := f.PropString(propGraphQLMountClass)
		if f.Kind != facts.KindRoute || class == "" {
			kept = append(kept, f)
			continue
		}
		resolved := ix.lexical(class, f.PropString(propGraphQLMountScope))
		gn := f.PropString(propGraphQLName)
		if gn == "" {
			gn = names[resolved]
		}
		if resolved == "" || gn == "" {
			dropped++
			continue
		}
		f.Name = "Mutation." + graphqlCamelize(railsUnderscore(gn))
		f.DelProp(propGraphQLMountClass)
		f.DelProp(propGraphQLMountScope)
		f.DelProp(propGraphQLName)
		if h := ix.member(resolved, "#", "resolve"); h != "" {
			f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelHandledBy, Target: h})
		}
		kept = append(kept, f)
	}
	if dropped > 0 {
		log.Printf("[ruby-extractor] graphql-ruby: %d mounted mutation(s) whose class or graphql_name could not be read", dropped)
	}
	return kept
}

var (
	underscoreAcronymRe = regexp.MustCompile(`([A-Z\d]+)([A-Z][a-z])`)
	underscoreWordRe    = regexp.MustCompile(`([a-z\d])([A-Z])`)
)

// railsUnderscore is ActiveSupport's String#underscore without inflection
// acronyms: "AuditEventsHTTPFilter" -> "audit_events_http_filter".
func railsUnderscore(s string) string {
	s = strings.ReplaceAll(s, "::", "/")
	s = underscoreAcronymRe.ReplaceAllString(s, "${1}_${2}")
	s = underscoreWordRe.ReplaceAllString(s, "${1}_${2}")
	return strings.ToLower(strings.ReplaceAll(s, "-", "_"))
}

func graphqlCamelize(s string) string {
	parts := strings.Split(s, "_")
	for i := 1; i < len(parts); i++ {
		if parts[i] != "" {
			parts[i] = strings.ToUpper(parts[i][:1]) + parts[i][1:]
		}
	}
	return strings.Join(parts, "")
}

func countNewlines(b []byte) int {
	n := 0
	for _, c := range b {
		if c == '\n' {
			n++
		}
	}
	return n
}
