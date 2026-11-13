// Package releaseaddresses joins an address in a release's values to the Argo CD
// Application serving it.
package releaseaddresses

import (
	"context"
	"regexp"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/plugin"
)

// Binder draws a depends_on edge from an Application to each Application serving an
// address its values state.
//
// The chart renders the Service an address names (`cart-api.cart.svc:8080`), so no
// Service object is there to resolve it to; the Application deploying it is. That
// Application often lives in another repository than the caller (one GitOps
// repository per platform, another generated per service), and an extractor reads one
// repository at a time, so the kubernetes extractor states both sides as props
// (addresses on one, serves and environment on the other) and this binder joins them
// over the whole union.
//
// The candidates are the Applications deploying to the address's namespace that serve
// its name. A cluster name resolves inside the caller's cluster, so those in the
// caller's own cluster answer. Failing that, a service mesh spanning clusters routes
// to a neighbour, so those in the caller's region and environment answer (region is
// the cluster name's leading letters and digits). Failing that, the one candidate
// there is, and never a pick among several: across clusters, several are usually
// production's and sandbox's.
type Binder struct{}

// New returns the binder.
func New() *Binder { return &Binder{} }

// Name identifies the binder.
func (b *Binder) Name() string { return "release-addresses" }

// Stage is pre-link: the edges it draws are symbol edges the module-edge roll-up and
// the cross-repo linker then read.
func (b *Binder) Stage() plugin.BindStage { return plugin.StagePreLink }

var regionPrefix = regexp.MustCompile(`^[a-z]+[0-9]+`)

type app struct {
	repo, name, cluster, namespace, env string
	serves                              map[string]bool
	addresses                           []string
}

type key struct{ repo, name string }

// Bind adds the edges.
func (b *Binder) Bind(_ context.Context, store *facts.Store) error {
	var apps []*app
	byNS := map[string][]*app{}
	for _, f := range store.FactsRef() {
		if f.Kind != facts.KindSymbol || f.PropString("k8s_kind") != "Application" || f.PropString("language") != "kubernetes" {
			continue
		}
		a := &app{
			repo: f.Repo, name: f.Name,
			cluster:   f.PropString("destination_cluster"),
			namespace: f.PropString("destination_namespace"),
			env:       f.PropString("environment"),
			serves:    map[string]bool{},
			addresses: stringList(f.PropAny("addresses")),
		}
		for _, s := range stringList(f.PropAny("serves")) {
			a.serves[s] = true
		}
		apps = append(apps, a)
		if a.namespace != "" {
			byNS[a.namespace] = append(byNS[a.namespace], a)
		}
	}
	adds := map[key][]string{}
	for _, a := range apps {
		set := map[string]bool{}
		for _, addr := range a.addresses {
			name, ns := parse(addr)
			for _, t := range resolve(a, name, byNS[ns]) {
				set[t.name] = true
			}
		}
		if len(set) > 0 {
			targets := make([]string, 0, len(set))
			for t := range set {
				targets = append(targets, t)
			}
			sort.Strings(targets)
			adds[key{a.repo, a.name}] = targets
		}
	}
	if len(adds) == 0 {
		return nil
	}
	store.UpdateWhere(func(f *facts.Fact) {
		targets := adds[key{f.Repo, f.Name}]
		if f.Kind != facts.KindSymbol || len(targets) == 0 {
			return
		}
		for _, t := range targets {
			if !has(f, t) {
				f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelDependsOn, Target: t})
			}
		}
	})
	return nil
}

// parse splits a recorded address, name.namespace[:port].
func parse(addr string) (name, ns string) {
	if i := strings.LastIndexByte(addr, ':'); i >= 0 {
		addr = addr[:i]
	}
	name, ns, _ = strings.Cut(addr, ".")
	return name, ns
}

func resolve(from *app, name string, inNS []*app) []*app {
	var cands, same, near []*app
	region := regionPrefix.FindString(from.cluster)
	for _, c := range inNS {
		if c == from || !c.serves[name] {
			continue
		}
		cands = append(cands, c)
		if c.cluster == from.cluster {
			same = append(same, c)
		}
		if region != "" && from.env != "" && regionPrefix.FindString(c.cluster) == region && c.env == from.env {
			near = append(near, c)
		}
	}
	switch {
	case len(same) > 0:
		return same
	case len(near) > 0:
		return near
	case len(cands) == 1:
		return cands
	}
	return nil
}

func has(f *facts.Fact, target string) bool {
	for _, r := range f.Relations {
		if r.Kind == facts.RelDependsOn && r.Target == target {
			return true
		}
	}
	return false
}

func stringList(v any) []string {
	switch x := v.(type) {
	case []string:
		return x
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
