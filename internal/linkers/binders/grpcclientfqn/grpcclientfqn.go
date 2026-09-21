// Package grpcclientfqn resolves provisional gRPC client routes to their fully
// qualified wire path.
package grpcclientfqn

import (
	"context"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/plugin"
)

// ProvisionalSources are the client-route sources that carry a SHORT service name and
// need the fully qualified one resolved from a .proto before they can be matched.
//
// The Python extractor is the one that needs this: a `SttServiceStub(channel)` call
// site names the service as "SttService", while the wire path the server serves is
// "/vosk.stt.v1.SttService/StreamingRecognize". The package qualifier exists only in
// the .proto, which a different extractor read.
//
// A table rather than an equality check because the property — "this extractor can
// only see the short name" — is a property of a language's gRPC codegen, not of
// Python. Adding a language is adding a row.
var ProvisionalSources = map[string]bool{
	facts.RouteSourcePythonGRPCClient: true,
}

// Binder rewrites provisional gRPC client routes to their fully qualified wire path,
// resolving the short service name against the gRPC SERVER routes in the store (which
// the grpc extractor read from the .proto).
//
// It is pre-link, and that is a correctness constraint rather than a preference: the
// cross-repo linker matches routes by Name, so a client route still carrying
// "/SttService/StreamingRecognize" when linking runs matches nothing, and the whole
// gRPC dependency is lost. This is the one ordering dependency in the original
// five-call sequence that actually mattered, and it looked identical to the three
// that did not.
//
// It re-resolves from the preserved grpc_service_short prop each run, so it is
// idempotent across appends. A client route whose service has no .proto in the
// snapshot — or whose short name is ambiguous — is left provisional rather than
// guessed at.
type Binder struct{}

// New returns the binder.
func New() *Binder { return &Binder{} }

func (b *Binder) Name() string { return "grpc-client-fqn" }

func (b *Binder) Stage() plugin.BindStage { return plugin.StagePreLink }

func (b *Binder) Bind(_ context.Context, store *facts.Store) error {
	// Proto index from server routes: short service name -> fq, plus per-fq method
	// sets and an ambiguity guard (two packages sharing a short service name).
	fqOf := map[string]string{}
	ambiguous := map[string]bool{}
	methodsOf := map[string]map[string]bool{}
	for _, r := range store.ByKind(facts.KindRoute) {
		if r.PropString(facts.PropRouteType) != facts.RouteTypeGRPC ||
			r.PropString(facts.PropRole) != facts.RoleServer {
			continue
		}
		fq := r.PropString("rpc_service")
		if fq == "" {
			continue
		}
		short := facts.ShortName(fq)
		if prev, ok := fqOf[short]; ok && prev != fq {
			ambiguous[short] = true
		} else {
			fqOf[short] = fq
		}
		if methodsOf[fq] == nil {
			methodsOf[fq] = map[string]bool{}
		}
		if m := r.PropString("rpc_method"); m != "" {
			methodsOf[fq][m] = true
		}
	}

	// Collect + resolve client routes, then remove-and-re-add so the store's name
	// index stays consistent (in-place Name mutation would desync byName).
	var replaced []facts.Fact
	found := false
	for _, r := range store.ByKind(facts.KindRoute) {
		if !ProvisionalSources[r.PropString(facts.PropSource)] {
			continue
		}
		found = true
		short := r.PropString("grpc_service_short")
		method := r.PropString("rpc_method")
		fq := fqOf[short]
		if short != "" && method != "" && fq != "" && !ambiguous[short] && methodsOf[fq][method] {
			r.Props = r.CloneProps()
			r.Name = "/" + fq + "/" + method
			r.SetProp("rpc_service", fq)
		}
		replaced = append(replaced, r)
	}
	if !found {
		return nil
	}
	store.RemoveWhere(func(f facts.Fact) bool {
		return f.Kind == facts.KindRoute && ProvisionalSources[f.PropString(facts.PropSource)]
	})
	store.Add(replaced...)
	return nil
}
