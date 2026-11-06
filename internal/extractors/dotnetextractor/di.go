package dotnetextractor

// Dependency-injection registration — the reference a DI-wired class actually has.
//
// A .NET application calls almost everything through an interface, and the only
// place the implementation is named is its registration in a startup file:
//
//	services.AddScoped<IOrderRepository, OrderRepository>();
//	services.AddHostedService<OrderProcessor>();
//
// Without reading those, `OrderRepository` has no inbound edge at all and reads as
// dead. Measured across the corpus: 441 of bitwarden-server's 1,661 orphan classes
// (27%) and 59 of eShop's 202 (29%) are named in a registration and nowhere else.
//
// This is a fix to the GRAPH rather than to a confidence heuristic, which is the
// right layer — the registration is a real reference that was simply not being
// read, not a false positive to be explained away afterwards.

import (
	sitter "github.com/enola-labs/enola/internal/treesitter"

	"github.com/enola-labs/enola/internal/facts"
)

// diRegistrars are the container methods whose TYPE ARGUMENTS name a service and
// its implementation.
//
// Listed explicitly rather than matched as `Add*`: a generic `AddRange<T>` or a
// fluent builder's `AddPolicy<T>` names no service, and treating every `Add`
// method as a registration would draw an edge from a startup file to whatever type
// happened to be passed anywhere in the application.
var diRegistrars = map[string]bool{
	"AddScoped": true, "AddSingleton": true, "AddTransient": true,
	"TryAddScoped": true, "TryAddSingleton": true, "TryAddTransient": true,
	"TryAddEnumerable": true,
	"AddHostedService": true, "AddDbContext": true, "AddDbContextPool": true,
	"AddDbContextFactory": true, "AddHttpClient": true, "AddGrpcClient": true,
	"AddSingletonAlias": true, "Decorate": true,
	// ASP.NET pipeline and framework registrations that also name a type.
	"UseMiddleware": true, "AddAuthenticationScheme": true, "AddOptions": true,
	"Configure": true, "AddHub": true, "MapHub": true,
}

// noteDIRegistration emits an instantiates edge to every type argument of a
// container registration.
//
// `instantiates` rather than `calls` because that is what the container does with
// the implementation, and because the orphan detector already treats an
// instantiates edge as a use — the same relation `new Foo()` produces.
func (w *astWalker) noteDIRegistration(node *sitter.Node) {
	fn := node.ChildByFieldName("function")
	if fn == nil {
		return
	}
	// The generic name is either the callee itself (`AddScoped<T>(…)`) or the
	// member being accessed (`services.AddScoped<T>(…)`).
	gen := fn
	if kindOf(fn) == "member_access_expression" {
		gen = fn.ChildByFieldName("name")
	}
	if gen == nil || kindOf(gen) != "generic_name" {
		return
	}
	nameNode := firstNamedChild(gen)
	if nameNode == nil || !diRegistrars[nodeText(nameNode, w.src)] {
		return
	}
	targs := findChildByKind(gen, "type_argument_list")
	if targs == nil {
		return
	}
	for i := uint(0); i < targs.NamedChildCount(); i++ {
		if t := w.targetForType(targs.NamedChild(i)); t != "" {
			w.addEdge(facts.RelInstantiates, t)
		}
	}
}
