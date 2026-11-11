// Package storedfuncs links a call through a stored function to the function stored.
//
// A Go function value handed to a constructor and kept in a field is called later,
// from a method of the type holding it, and nothing in the source names it there:
//
//	func newOrdersCache(api client) *ordersCache {
//		fetch := func(ctx context.Context, ids []int) (map[int]Order, error) { return api.Orders(ctx, ids) }
//		return &ordersCache{rt: cache.NewReadThrough(store, fetch)}
//	}
//	func (c *ordersCache) get(ctx context.Context, ids []int) { c.rt.GetMany(ctx, ids) }
//
// GetMany calls r.source, the field fetch went into, so get runs fetch, and fetch
// calls the client. The Go extractor records each step where it happens (see
// goextractor/funcflow.go); this binder joins them.
//
// The join is made per holder field, not per type. ordersCache.rt was filled by a call
// that handed fetch to NewReadThrough's parameter 1, and that parameter reaches the
// field ReadThrough.source, through any constructor it is forwarded to. GetMany calls
// ReadThrough.source, itself or through another method of ReadThrough. So a method
// calling GetMany on ordersCache.rt calls fetch: that call edge is added. Every other
// read-through cache of the program holds its own fetch in the same field, and joining
// per type would have made each of them call all of them.
//
// A field no holder is filled through, set where its struct is built
// (`&T{run: func() {...}}`) and called by a method of T, is joined by type, when it
// holds a few functions only: see linkByType.
package storedfuncs

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/plugin"
)

// PropFuncFlows is the Go extractor's prop the binder reads.
const PropFuncFlows = "func_flows"

type Binder struct{}

func New() *Binder { return &Binder{} }

func (b *Binder) Name() string { return "stored-funcs" }

// Pre-link: the edges are within one repository, and everything built on the graph
// afterwards should see them.
func (b *Binder) Stage() plugin.BindStage { return plugin.StagePreLink }

// slot is where a function value can be: parameter i of a function ("P fn#i") or a
// field of a type ("F T.f"), in one repository.
type slot struct{ repo, name string }

type holder struct {
	repo, self, field, callee string
	args                      map[int]string // parameter index -> value
}

type holderCall struct{ repo, self, field, method string }

type index struct {
	edges      map[slot][]slot   // a parameter forwarded to a parameter or a field
	preds      map[slot][]slot   // edges, reversed
	values     map[slot][]string // concrete values passed or stored
	holders    []holder
	fieldCalls map[slot][]string // method -> fields it calls ("F T.f" names)
	sameType   map[slot][]string // method -> methods of its own type it calls
	calls      []holderCall
}

func (b *Binder) Bind(_ context.Context, store *facts.Store) error {
	ix := build(store.FactsRef())
	if len(ix.calls) == 0 && len(ix.fieldCalls) == 0 {
		return nil
	}
	adds := link(ix)
	if len(adds) == 0 {
		return nil
	}
	store.UpdateWhere(func(f *facts.Fact) {
		targets := adds[slot{f.Repo, f.Name}]
		if f.Kind != facts.KindSymbol || len(targets) == 0 {
			return
		}
		for _, t := range targets {
			if !hasCall(f, t) {
				f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelCalls, Target: t})
			}
		}
	})
	return nil
}

func hasCall(f *facts.Fact, target string) bool {
	for _, r := range f.Relations {
		if r.Kind == facts.RelCalls && r.Target == target {
			return true
		}
	}
	return false
}

// build reads every func_flows entry, and which methods call methods of their own
// type.
func build(all []facts.Fact) *index {
	ix := &index{
		edges: map[slot][]slot{}, preds: map[slot][]slot{}, values: map[slot][]string{},
		fieldCalls: map[slot][]string{}, sameType: map[slot][]string{},
	}
	for _, f := range all {
		if f.Kind != facts.KindSymbol {
			continue
		}
		self := slot{f.Repo, f.Name}
		if f.PropString("symbol_kind") == facts.SymbolMethod {
			owner := f.Name[:strings.LastIndex(f.Name, ".")+1]
			for _, r := range f.Relations {
				if r.Kind == facts.RelCalls && strings.HasPrefix(r.Target, owner) && !strings.Contains(r.Target[len(owner):], ".") {
					ix.sameType[self] = append(ix.sameType[self], r.Target)
				}
			}
		}
		for _, e := range stringList(f.PropAny(PropFuncFlows)) {
			ix.read(f.Repo, f.Name, e)
		}
	}
	return ix
}

// read takes in one func_flows entry of function self.
func (ix *index) read(repo, self, e string) {
	kind, rest, _ := strings.Cut(e, " ")
	switch kind {
	case "arg", "field":
		dst, v, ok := strings.Cut(rest, " <- ")
		if !ok {
			return
		}
		to := slot{repo, "P " + dst}
		if kind == "field" {
			to = slot{repo, "F " + dst}
		}
		if j, isParam := param(v); isParam {
			from := slot{repo, "P " + self + "#" + strconv.Itoa(j)}
			ix.edges[from] = append(ix.edges[from], to)
			ix.preds[to] = append(ix.preds[to], from)
			return
		}
		ix.values[to] = append(ix.values[to], v)
	case "holder":
		field, call, ok := strings.Cut(rest, " <- ")
		open := strings.Index(call, "(")
		if !ok || open < 0 || !strings.HasSuffix(call, ")") {
			return
		}
		h := holder{repo: repo, self: self, field: field, callee: call[:open], args: map[int]string{}}
		for _, a := range strings.Split(call[open+1:len(call)-1], ",") {
			i, v, ok := strings.Cut(a, "=")
			if n, err := strconv.Atoi(i); ok && err == nil {
				h.args[n] = v
			}
		}
		ix.holders = append(ix.holders, h)
	case "fieldcall":
		m := slot{repo, self}
		ix.fieldCalls[m] = append(ix.fieldCalls[m], "F "+rest)
	case "holdercall":
		field, method, ok := strings.Cut(rest, " -> ")
		if ok {
			ix.calls = append(ix.calls, holderCall{repo, self, field, method})
		}
	}
}

func param(v string) (int, bool) {
	if !strings.HasPrefix(v, "@") {
		return 0, false
	}
	j, err := strconv.Atoi(v[1:])
	return j, err == nil
}

// link returns the calls to add, by caller: each method called on a holder field
// calls the values that field's instance holds in the fields the method calls.
func link(ix *index) map[slot][]string {
	// What each holder field's instance holds, by the field of its own type.
	inst := map[slot]map[string][]string{}
	for _, h := range ix.holders {
		hk := slot{h.repo, h.field}
		for i, v := range h.args {
			vals := []string{v}
			if j, isParam := param(v); isParam {
				vals = ix.valuesOf(slot{h.repo, "P " + h.self + "#" + strconv.Itoa(j)})
			}
			for _, f := range ix.fieldsReached(slot{h.repo, "P " + h.callee + "#" + strconv.Itoa(i)}) {
				if inst[hk] == nil {
					inst[hk] = map[string][]string{}
				}
				inst[hk][f] = append(inst[hk][f], vals...)
			}
		}
	}
	adds := map[slot][]string{}
	ix.linkByType(inst, adds)
	memo := map[slot][]string{}
	for _, c := range ix.calls {
		held := inst[slot{c.repo, c.field}]
		if held == nil {
			continue
		}
		caller := slot{c.repo, c.self}
		for _, f := range ix.invokes(slot{c.repo, c.method}, memo, map[slot]bool{}) {
			adds[caller] = append(adds[caller], held[f]...)
		}
	}
	for k, v := range adds {
		sort.Strings(v)
		adds[k] = compact(v)
	}
	return adds
}

// maxTypedValues is the most functions a field may hold to be linked by its type.
const maxTypedValues = 5

// linkByType links a call through a field that no holder is filled through, the
// method making it calling every function stored in that field anywhere: one stored
// in a struct literal or by assignment (`&T{run: func() {...}}`, `t.run = fn`), or
// handed to the parameter that is. Such a field is mostly set in one place. A field
// holding more than maxTypedValues functions is a dispatch point, every call of
// which would reach all of them, and is left unlinked. A field a holder is filled
// through is linked per instance by link, never by type.
func (ix *index) linkByType(inst map[slot]map[string][]string, adds map[slot][]string) {
	perInstance := map[slot]bool{}
	for hk, fields := range inst {
		for f := range fields {
			perInstance[slot{hk.repo, f}] = true
		}
	}
	for m, fields := range ix.fieldCalls {
		for _, f := range fields {
			field := slot{m.repo, f}
			if perInstance[field] {
				continue
			}
			vals := ix.valuesOf(field)
			sort.Strings(vals)
			vals = compact(vals)
			if len(vals) > 0 && len(vals) <= maxTypedValues {
				adds[m] = append(adds[m], vals...)
			}
		}
	}
}

// fieldsReached returns the fields a parameter's value is stored in, through every
// function it is forwarded to.
func (ix *index) fieldsReached(from slot) []string {
	var out []string
	seen := map[slot]bool{from: true}
	queue := []slot{from}
	for len(queue) > 0 {
		s := queue[0]
		queue = queue[1:]
		for _, next := range ix.edges[s] {
			if seen[next] {
				continue
			}
			seen[next] = true
			if strings.HasPrefix(next.name, "F ") {
				out = append(out, next.name)
				continue
			}
			queue = append(queue, next)
		}
	}
	return out
}

// valuesOf returns the concrete values that reach a parameter, through every function
// forwarding its own parameter to it.
func (ix *index) valuesOf(to slot) []string {
	var out []string
	seen := map[slot]bool{to: true}
	queue := []slot{to}
	for len(queue) > 0 {
		s := queue[0]
		queue = queue[1:]
		out = append(out, ix.values[s]...)
		for _, p := range ix.preds[s] {
			if !seen[p] {
				seen[p] = true
				queue = append(queue, p)
			}
		}
	}
	return out
}

// invokes returns the fields a method calls, itself or through methods of its own
// type it calls.
func (ix *index) invokes(m slot, memo map[slot][]string, visiting map[slot]bool) []string {
	if got, ok := memo[m]; ok {
		return got
	}
	if visiting[m] {
		return nil
	}
	visiting[m] = true
	out := append([]string(nil), ix.fieldCalls[m]...)
	for _, callee := range ix.sameType[m] {
		out = append(out, ix.invokes(slot{m.repo, callee}, memo, visiting)...)
	}
	sort.Strings(out)
	out = compact(out)
	memo[m] = out
	return out
}

func compact(s []string) []string {
	if len(s) < 2 {
		return s
	}
	out := s[:1]
	for _, v := range s[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
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
