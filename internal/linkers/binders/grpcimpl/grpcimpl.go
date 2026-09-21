// Package grpcimpl binds each gRPC server route to the method that implements it.
package grpcimpl

import (
	"context"
	"log"
	"regexp"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/plugin"
)

// ServerBase is one code generator's convention for the base type a gRPC server
// implementation derives from. Matching that type's name is what bridges a route
// read from a .proto to the method serving it: the two are extracted by different
// extractors that never see each other, and the generated base type is the only
// thing naming the service on the implementation side.
//
// Pattern must capture the service SHORT NAME in group 1, and should tolerate a
// package or alias qualifier ("usersv1.UnimplementedUserServiceServer") because the
// extractors record the target as written at the embed/inherit site.
type ServerBase struct {
	// Generator names the tool whose convention this is, for documentation and to
	// make an added row self-describing.
	Generator string
	Pattern   *regexp.Regexp
}

// DefaultServerBases are the generator conventions recognized out of the box. This
// list is the whole of the language-specific knowledge in this binder: supporting a
// new gRPC codegen means adding a row, not editing the binding logic, which is why
// it is a table and not a pair of if-statements.
//
// It is exported and read at construction time so out-of-tree code can extend it via
// NewWith rather than forking the binder.
var DefaultServerBases = []ServerBase{
	{
		// protoc-gen-go-grpc forward-compatibility base, embedded by a Go impl:
		// "usersv1.UnimplementedUserServiceServer" -> "UserService".
		Generator: "protoc-gen-go-grpc",
		Pattern:   regexp.MustCompile(`^(?:.*\.)?Unimplemented(.+)Server$`),
	},
	{
		// grpc_python_out servicer base, subclassed by a Python impl:
		// "stt_service_pb2_grpc.SttServiceServicer" -> "SttService".
		Generator: "grpc_python_out",
		Pattern:   regexp.MustCompile(`^(?:.*\.)?(.+)Servicer$`),
	},
}

// Binder connects each gRPC server route (emitted from a .proto by the grpc
// extractor) to the method that implements it, so route -> handler is traversable by
// impact_analysis and find_path.
//
// The bridge is a code generator's naming convention: an implementation embeds or
// subclasses a generated base type, which the language extractor already records as
// an `implements` edge. That type's short name ("UserService") equals the last
// segment of the route's rpc_service ("users.v1.UserService"), so the route's
// rpc_method maps to the implementation's method symbol.
//
// It is post-link because it neither reads nor feeds cross-repo linking: routes and
// implementations are matched strictly within one repo.
type Binder struct {
	bases []ServerBase
}

// New returns a binder using DefaultServerBases.
func New() *Binder { return NewWith(DefaultServerBases) }

// NewWith returns a binder recognizing exactly the given conventions.
func NewWith(bases []ServerBase) *Binder {
	return &Binder{bases: bases}
}

func (b *Binder) Name() string { return "grpc-impl" }

func (b *Binder) Stage() plugin.BindStage { return plugin.StagePostLink }

func (b *Binder) Bind(_ context.Context, store *facts.Store) error {
	symbols := store.ByKind(facts.KindSymbol)

	// Per-repo index of service short name -> implementation symbol name, plus the
	// set of method symbol names (for existence checks). Both are scoped by repo so
	// a route only ever binds to an implementation in its own repo.
	implMap := map[string]map[string]string{} // repo -> shortName -> impl name
	ambiguous := map[string]map[string]bool{} // repo -> shortName -> seen twice
	methodSet := map[string]map[string]bool{} // repo -> method name -> exists

	for _, s := range symbols {
		kind, _ := s.PropAny("symbol_kind").(string)
		switch kind {
		case facts.SymbolStruct, facts.SymbolClass:
			short := b.implShortName(s)
			if short == "" {
				continue
			}
			if implMap[s.Repo] == nil {
				implMap[s.Repo] = map[string]string{}
				ambiguous[s.Repo] = map[string]bool{}
			}
			if existing, ok := implMap[s.Repo][short]; ok && existing != s.Name {
				ambiguous[s.Repo][short] = true
			} else {
				implMap[s.Repo][short] = s.Name
			}
		case facts.SymbolMethod:
			if methodSet[s.Repo] == nil {
				methodSet[s.Repo] = map[string]bool{}
			}
			methodSet[s.Repo][s.Name] = true
		}
	}

	bound := 0
	store.UpdateWhere(func(f *facts.Fact) {
		if f.Kind != facts.KindRoute || f.Props == nil {
			return
		}
		if f.PropAny(facts.PropRouteType) != facts.RouteTypeGRPC || f.PropAny(facts.PropRole) != facts.RoleServer {
			return
		}
		short := facts.ShortName(f.PropString("rpc_service"))
		method := f.PropString("rpc_method")
		if short == "" || method == "" {
			return
		}
		if ambiguous[f.Repo][short] {
			return // two impls claim this service short name — don't guess
		}
		impl := implMap[f.Repo][short]
		if impl == "" {
			return
		}
		target := impl + "." + method
		if !methodSet[f.Repo][target] {
			return
		}
		if f.HasRelation(facts.RelHandledBy, target) {
			return // idempotent across appends
		}
		f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelHandledBy, Target: target})
		f.SetProp("handler", target)
		bound++
	})
	if bound > 0 {
		log.Printf("[binder:grpc-impl] bound %d gRPC server route(s) to their handler", bound)
	}
	return nil
}

// implShortName returns the gRPC service short name a symbol implements, by testing
// each `implements` target against every registered generator convention, or "" if it
// implements no such type.
func (b *Binder) implShortName(s facts.Fact) string {
	for _, r := range s.Relations {
		if r.Kind != facts.RelImplements {
			continue
		}
		for _, base := range b.bases {
			if m := base.Pattern.FindStringSubmatch(r.Target); m != nil {
				return m[1]
			}
		}
	}
	return ""
}
