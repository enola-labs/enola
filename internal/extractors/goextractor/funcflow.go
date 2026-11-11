package goextractor

import (
	"go/ast"
	"sort"
	"strconv"
	"strings"
)

// A function value handed over and called later leaves no call edge where it is
// called. The common shape:
//
//	func newOrdersCache(api client) *ordersCache {
//		fetch := func(ctx context.Context, ids []int) (map[int]Order, error) { return api.Orders(ctx, ids) }
//		return &ordersCache{rt: cache.NewReadThrough(store, fetch)}
//	}
//	func (c *ordersCache) get(ctx context.Context, ids []int) { c.rt.GetMany(ctx, ids) }
//
// where NewReadThrough stores fetch in a field and GetMany calls it. Nothing links
// get to fetch, or to api.Orders behind it, so a change to the client reached no
// caller and no endpoint.
//
// The extractor records, per function, where function values go: the arguments it
// passes, the fields it stores them in, the holder fields it fills from a constructor,
// the fields it calls and the methods it calls on a holder. These are the
// propFuncFlows entries. The storedfuncs binder joins them across packages per
// holder field, so get reaches the fetch its own cache was built with and not every
// fetch any read-through cache holds.

// propFuncFlows lists a Go function's function-value flows, each one of:
//
//	arg <callee>#<i> <- <value>      a value passed as argument i
//	field <T>.<f> <- <value>          a value stored in field f of T
//	holder <H>.<g> <- <callee>(<i>=<value>,...)   field g of H set from a call
//	fieldcall <T>.<f>                 a call through field f of T
//	holdercall <H>.<g> -> <T>.<M>     a call of method M on field g of H
//
// A value is a function's symbol name, a closure's, or @<j> for the function's own
// parameter j.
const propFuncFlows = "func_flows"

// funcFieldType is the type recorded for a struct field of a function type, which has
// no name to record. fieldType does not return it: it says only that the field exists.
const funcFieldType = "\x00func"

// flowWalker reads one function's function-value flows.
type flowWalker struct {
	ctx      resolveCtx
	self     string
	params   map[string]int
	closures map[*ast.FuncLit]string // every literal in the body, by name
	localFns map[string]string       // local variable -> closure it holds
	calls    map[string]*ast.CallExpr
	flows    []string
	used     map[string]bool // closures that flow somewhere
}

// funcFlows returns a function's propFuncFlows entries and the closures they name,
// each with the literal it is. A closure is named for its function and its place
// among the function's literals, in source order: "pkg.fn.func1".
func funcFlows(fn *ast.FuncDecl, ctx resolveCtx, self string) ([]string, map[string]*ast.FuncLit) {
	if fn.Body == nil {
		return nil, nil
	}
	w := &flowWalker{
		ctx: ctx, self: self,
		params:   funcValueParams(fn.Type, ctx),
		closures: map[*ast.FuncLit]string{},
		localFns: map[string]string{},
		calls:    map[string]*ast.CallExpr{},
		used:     map[string]bool{},
	}
	n := 0
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		if lit, ok := node.(*ast.FuncLit); ok {
			n++
			w.closures[lit] = self + ".func" + strconv.Itoa(n)
		}
		return true
	})
	ast.Inspect(fn.Body, w.visit)

	lits := map[string]*ast.FuncLit{}
	for lit, name := range w.closures {
		if w.used[name] {
			lits[name] = lit
		}
	}
	sort.Strings(w.flows)
	return dedupSorted(w.flows), lits
}

func (w *flowWalker) visit(node ast.Node) bool {
	switch x := node.(type) {
	case *ast.AssignStmt:
		if len(x.Lhs) != len(x.Rhs) {
			return true
		}
		for i, lhs := range x.Lhs {
			w.assign(lhs, x.Rhs[i])
		}
	case *ast.ValueSpec:
		if len(x.Names) == len(x.Values) {
			for i, name := range x.Names {
				w.assign(name, x.Values[i])
			}
		}
	case *ast.CompositeLit:
		t := compositeLitType(x, w.ctx)
		if t == "" || !w.internal(t) {
			return true
		}
		for _, el := range x.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if key, ok := kv.Key.(*ast.Ident); ok {
				w.store(t, key.Name, kv.Value)
			}
		}
	case *ast.CallExpr:
		w.call(x)
	}
	return true
}

// assign reads `lhs = rhs`: a local bound to a closure or to a constructor call, or
// a value stored in a field.
func (w *flowWalker) assign(lhs, rhs ast.Expr) {
	switch l := lhs.(type) {
	case *ast.Ident:
		switch r := ast.Unparen(rhs).(type) {
		case *ast.FuncLit:
			w.localFns[l.Name] = w.closures[r]
		case *ast.CallExpr:
			w.calls[l.Name] = r
		}
	case *ast.SelectorExpr:
		if owner := exprType(l.X, w.ctx); owner != "" && w.internal(owner) && w.isField(owner, l.Sel.Name) {
			w.store(owner, l.Sel.Name, rhs)
		}
	}
}

// store records value going into field f of t: a constructor's result, which makes
// the field a holder, or a function value.
func (w *flowWalker) store(t, f string, value ast.Expr) {
	call, _ := ast.Unparen(value).(*ast.CallExpr)
	if id, ok := ast.Unparen(value).(*ast.Ident); ok && call == nil {
		call = w.calls[id.Name]
	}
	if call != nil {
		if callee := w.callee(call); callee != "" {
			var args []string
			for i, a := range call.Args {
				if v := w.value(a); v != "" {
					args = append(args, strconv.Itoa(i)+"="+v)
				}
			}
			if len(args) > 0 {
				w.flows = append(w.flows, "holder "+t+"."+f+" <- "+callee+"("+strings.Join(args, ",")+")")
			}
		}
		return
	}
	if v := w.value(value); v != "" {
		w.flows = append(w.flows, "field "+t+"."+f+" <- "+v)
	}
}

// call reads a call: the function values passed to a function of the module, a call
// through a field, and a method called on a field.
func (w *flowWalker) call(x *ast.CallExpr) {
	if callee := w.callee(x); callee != "" && w.internal(callee) {
		for i, a := range x.Args {
			if v := w.value(a); v != "" {
				w.flows = append(w.flows, "arg "+callee+"#"+strconv.Itoa(i)+" <- "+v)
			}
		}
	}
	sel, ok := x.Fun.(*ast.SelectorExpr)
	if !ok {
		return
	}
	if owner := exprType(sel.X, w.ctx); owner != "" && w.internal(owner) && w.isField(owner, sel.Sel.Name) {
		w.flows = append(w.flows, "fieldcall "+owner+"."+sel.Sel.Name)
		return
	}
	inner, ok := sel.X.(*ast.SelectorExpr)
	if !ok {
		return
	}
	holder := exprType(inner.X, w.ctx)
	if holder == "" || !w.internal(holder) {
		return
	}
	t, ok := fieldType(holder, inner.Sel.Name, w.ctx)
	if !ok || !w.internal(t) || w.isField(t, sel.Sel.Name) {
		return
	}
	w.flows = append(w.flows, "holdercall "+holder+"."+inner.Sel.Name+" -> "+t+"."+sel.Sel.Name)
}

// callee is the function a call resolves to, as analyzeBody resolves it.
func (w *flowWalker) callee(x *ast.CallExpr) string {
	fun := x.Fun
	if idx, ok := fun.(*ast.IndexExpr); ok {
		if id, ok := idx.X.(*ast.Ident); ok && w.ctx.pkgFuncs[id.Name] {
			fun = idx.X
		}
	}
	return callTarget(fun, w.ctx)
}

// value is the function value an expression is, or "": a closure, a local holding
// one, a parameter of a function type, a function or a method value of the module.
func (w *flowWalker) value(e ast.Expr) string {
	switch x := ast.Unparen(e).(type) {
	case *ast.FuncLit:
		name := w.closures[x]
		w.used[name] = true
		return name
	case *ast.Ident:
		if name, ok := w.localFns[x.Name]; ok {
			w.used[name] = true
			return name
		}
		if j, ok := w.params[x.Name]; ok {
			return "@" + strconv.Itoa(j)
		}
		if w.ctx.moduleFuncs[w.ctx.pkgDir+"."+x.Name] && w.ctx.localTypes[x.Name] == "" {
			return w.ctx.pkgDir + "." + x.Name
		}
	case *ast.SelectorExpr:
		if id, ok := x.X.(*ast.Ident); ok && w.ctx.localTypes[id.Name] == "" && id.Name != w.ctx.recvVar {
			if imp, ok := w.ctx.imports[id.Name]; ok {
				if name := imp + "." + x.Sel.Name; w.ctx.moduleFuncs[name] {
					return name
				}
				return ""
			}
		}
		if t := exprType(x.X, w.ctx); t != "" && w.ctx.moduleMethods[t+"."+x.Sel.Name] {
			return t + "." + x.Sel.Name // a method value
		}
	}
	return ""
}

// isField reports whether name is a field of type t, of any type.
func (w *flowWalker) isField(t, name string) bool {
	_, ok := w.ctx.fieldTypes[t+"."+name]
	return ok
}

// internal reports whether a qualified name, a function, a type or a method, belongs
// to a package of the module.
func (w *flowWalker) internal(name string) bool {
	for range 3 {
		dot := strings.LastIndex(name, ".")
		if dot <= 0 {
			return false
		}
		name = name[:dot]
		if name == w.ctx.pkgDir {
			return true
		}
		if w.ctx.localPkgs != nil {
			if _, ok := w.ctx.localPkgs[name]; ok {
				return true
			}
		}
	}
	return false
}

// funcValueParams indexes the parameters that hold a function: one of a function
// type, or of a named type of the module that is one (`source Source[K, V]`).
func funcValueParams(ft *ast.FuncType, ctx resolveCtx) map[string]int {
	out := map[string]int{}
	if ft.Params == nil {
		return out
	}
	i := 0
	for _, field := range ft.Params.List {
		holds := false
		if _, ok := field.Type.(*ast.FuncType); ok {
			holds = true
		} else if name := typeExprToString(field.Type); name != "" && !strings.HasPrefix(name, "*") {
			holds = ctx.funcTypes[resolveTypeName(name, ctx)]
		}
		if len(field.Names) == 0 {
			i++
			continue
		}
		for _, n := range field.Names {
			if holds {
				out[n.Name] = i
			}
			i++
		}
	}
	return out
}

func dedupSorted(s []string) []string {
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

// collectFuncTypes returns the names of a package's types whose underlying type is a
// function type.
func collectFuncTypes(files []*ast.File) map[string]bool {
	out := map[string]bool{}
	for _, f := range files {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gd.Specs {
				if ts, ok := spec.(*ast.TypeSpec); ok {
					if _, isFunc := ts.Type.(*ast.FuncType); isFunc {
						out[ts.Name.Name] = true
					}
				}
			}
		}
	}
	return out
}
