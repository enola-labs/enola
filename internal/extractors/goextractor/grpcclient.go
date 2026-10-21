package goextractor

import (
	"go/ast"
	"go/token"
	"regexp"
	"strconv"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// Go gRPC client detection.
//
// A Go consumer of a gRPC service writes:
//
//	client := usersv1.NewUserServiceClient(conn)  // grpc-go
//	c := usersv1connect.NewUserServiceClient(h, u) // connect-go
//	resp, _ := client.GetUser(ctx, req)
//
// The call site carries no wire path; the authoritative "/pkg.Service/Method"
// lives only as a string literal in the *generated* code — inside the concrete
// grpc-go client's `.Invoke`/`.NewStream` call, or as a connect-go
// `…Procedure` const. So detection is two-phase:
//   1. buildGoGRPCStubIndex scans generated files repo-wide for those path
//      literals, keyed by the client interface name (<Service>Client).
//   2. extractGRPCClientFacts resolves each call receiver's type via the Go
//      extractor's own chain resolution (local vars, struct fields, and
//      receivers all handled) and emits a client-role route when the resolved
//      client interface + method are in the index.

// goGRPCStub is one resolved gRPC client: its methods mapped to wire paths.
//
// ambiguous records the methods that must never be emitted. The index is keyed
// by the short service name, so a service declared in two proto packages —
// ordinary API-versioning practice — collides here. Keeping the last writer
// would answer confidently from a derivation that failed, and would answer
// differently from run to run.
type goGRPCStub struct {
	methods   map[string]string // Go method name → "/fq.Service/Method"
	ambiguous map[string]bool
}

// goGRPCStubIndex maps an exported client interface name (e.g. "UserServiceClient",
// the type a consumer's NewUserServiceClient(...) binding resolves to) to its
// methods.
type goGRPCStubIndex struct {
	byClient map[string]*goGRPCStub
}

func (idx *goGRPCStubIndex) empty() bool { return idx == nil || len(idx.byClient) == 0 }

// grpcProcedurePath matches a gRPC full-method path "/pkg.Service/Method": two
// slash-delimited segments, the service segment allowing the proto package dots.
var grpcProcedurePath = regexp.MustCompile(`^/[A-Za-z_][A-Za-z0-9_.]*/[A-Za-z_][A-Za-z0-9_]*$`)

// buildGoGRPCStubIndex scans every parsed Go file that imports grpc/connect for
// procedure-path string literals and returns the client-interface→method→path
// index. It reads only the already-parsed ASTs. Returns nil when nothing found.
//
// Keying by "<serviceShort>Client" (derived from the path) is exactly what a
// consumer's NewXxxClient(...) binding resolves to for both protoc-gen-go-grpc
// and protoc-gen-connect-go, so the same index serves both libraries.
func buildGoGRPCStubIndex(files []*ast.File) *goGRPCStubIndex {
	idx := &goGRPCStubIndex{byClient: map[string]*goGRPCStub{}}

	for _, f := range files {
		if !looksLikeGoGRPCGenerated(f) {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil || !grpcProcedurePath.MatchString(s) {
				return true
			}
			iface := afterLastDot(grpcServiceOf(s)) + "Client"
			stub := idx.byClient[iface]
			if stub == nil {
				stub = &goGRPCStub{methods: map[string]string{}, ambiguous: map[string]bool{}}
				idx.byClient[iface] = stub
			}
			method := grpcMethodOf(s)
			if stub.ambiguous[method] {
				return true
			}
			if existing, present := stub.methods[method]; present && existing != s {
				// Two proto packages declare this service. Which one a call
				// site means cannot be derived from the short name, so nothing
				// is emitted: a missing edge is honest, a guessed one is not.
				stub.ambiguous[method] = true
				delete(stub.methods, method)
				return true
			}
			stub.methods[method] = s
			return true
		})
	}

	if len(idx.byClient) == 0 {
		return nil
	}
	return idx
}

// looksLikeGoGRPCGenerated reports whether a file imports the gRPC or connect
// runtime, gating the procedure-literal scan to generated stubs (and consumer
// files, which harmlessly contain no procedure literals) rather than arbitrary
// sources with incidental "/a/b" strings.
func looksLikeGoGRPCGenerated(f *ast.File) bool {
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		if strings.HasPrefix(p, "google.golang.org/grpc") ||
			strings.Contains(p, "connectrpc.com/connect") ||
			strings.Contains(p, "bufbuild/connect-go") {
			return true
		}
	}
	return false
}

// collectPackageVarClients maps a package-level variable name to the gRPC client
// interface it is constructed from — e.g. `var userClient = usersv1.NewUserServiceClient(conn)`
// yields "userClient" → "UserServiceClient". It scans every file's top-level var
// declarations (extractGenDecl ignores value specs, and collectLocalTypes only
// sees function bodies, so package vars are otherwise unresolved). Only vars
// whose interface exists in the stub index are kept. Package-scoped, so a var
// declared in one file resolves for calls in another file of the same package.
func collectPackageVarClients(files []*ast.File, idx *goGRPCStubIndex) map[string]string {
	if idx.empty() {
		return nil
	}
	out := map[string]string{}
	for _, f := range files {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					if iface := clientCtorInterface(vs.Values[i]); iface != "" && idx.byClient[iface] != nil {
						out[name.Name] = iface
					}
				}
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// extractGRPCClientFacts emits a client-role route for each gRPC client call site
// in a file. It rebuilds the extractor's resolveCtx per function (exactly as
// extractFunc does) so it can reuse resolveChain — which resolves a call
// receiver whether it is a local variable, a struct field, or the method
// receiver — then looks the resolved client interface up in the stub index.
// pkgVarClients supplies package-level var receivers, which resolveChain cannot
// resolve (they live in no function body).
func extractGRPCClientFacts(fset *token.FileSet, f *ast.File, relFile, pkgDir string, mods goModules, fileImports, fieldTypes, pkgVarClients map[string]string, idx *goGRPCStubIndex) []facts.Fact {
	if idx.empty() {
		return nil
	}

	var out []facts.Fact
	seen := map[string]bool{}
	emit := func(iface, method string, pos token.Pos) {
		stub := idx.byClient[iface]
		if stub == nil {
			return
		}
		path := stub.methods[method]
		if path == "" {
			return
		}
		line := fset.Position(pos).Line
		key := path + "\x00" + strconv.Itoa(line)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, facts.Fact{
			Kind: facts.KindRoute,
			Name: path,
			File: relFile,
			Line: line,
			Props: map[string]any{
				facts.PropRole:      facts.RoleClient,
				"method":            "POST",
				facts.PropFramework: facts.FrameworkGRPC,
				"language":          "go",
				facts.PropSource:    facts.RouteSourceGoGRPCClient,
				"type":              "grpc",
				"rpc_service":       grpcServiceOf(path),
				"rpc_method":        grpcMethodOf(path),
			},
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: pkgDir}},
		})
	}

	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		ctx := resolveCtx{
			pkgDir:     pkgDir,
			mods:       mods,
			imports:    fileImports,
			fieldTypes: fieldTypes,
		}
		if fn.Recv != nil && len(fn.Recv.List) > 0 {
			ctx.recvType = receiverTypeName(fn.Recv.List[0].Type)
			if names := fn.Recv.List[0].Names; len(names) > 0 {
				ctx.recvVar = names[0].Name
			}
		}
		ctx.localTypes = collectLocalTypes(fn.Body, ctx)

		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			// Package-level var receiver: `userClient.M()` where userClient is a
			// package-scoped client var (resolveChain can't resolve it — it lives
			// in no function body).
			if recv, ok := sel.X.(*ast.Ident); ok {
				if iface := pkgVarClients[recv.Name]; iface != "" {
					emit(iface, sel.Sel.Name, call.Pos())
					return true
				}
			}
			// Resolve the receiver's type via the extractor's chain resolution:
			// handles `client.M()` (local var), `s.field.M()` (struct field), and
			// `recv.M()` on the method receiver.
			if chain := flattenSelector(call.Fun); len(chain) >= 2 {
				method := chain[len(chain)-1]
				resolved := resolveChain(chain, ctx)
				iface := afterLastDot(strings.TrimSuffix(resolved, "."+method))
				if stub := idx.byClient[iface]; stub != nil && stub.methods[method] != "" {
					emit(iface, method, call.Pos())
					return true
				}
			}
			// Inline construction: pkg.NewXxxClient(conn).Method(...) — flattenSelector
			// returns nil for a call-on-call, so handle it directly.
			if inner, ok := sel.X.(*ast.CallExpr); ok {
				if iface := clientCtorInterface(inner); iface != "" {
					emit(iface, sel.Sel.Name, call.Pos())
				}
			}
			return true
		})
	}
	return out
}

// clientCtorInterface returns the client interface name a `NewXxxClient(...)`
// constructor call yields (e.g. "UserServiceClient"), or "" if expr is not such
// a call. Handles both `NewXxxClient(...)` and `pkg.NewXxxClient(...)`.
func clientCtorInterface(expr ast.Expr) string {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return ""
	}
	var name string
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		name = fn.Name
	case *ast.SelectorExpr:
		name = fn.Sel.Name
	default:
		return ""
	}
	rest := strings.TrimPrefix(name, "New")
	if rest == name || !strings.HasSuffix(rest, "Client") || rest == "Client" {
		return ""
	}
	return rest
}

// receiverTypeName returns the bare type name of a method receiver, unwrapping a
// pointer receiver (`*userServiceClient` → "userServiceClient").
func receiverTypeName(expr ast.Expr) string {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if id, ok := expr.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

// grpcServiceOf returns "users.v1.UserService" from "/users.v1.UserService/GetUser".
func grpcServiceOf(path string) string {
	p := strings.TrimPrefix(path, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i]
	}
	return p
}

// grpcMethodOf returns "GetUser" from "/users.v1.UserService/GetUser".
func grpcMethodOf(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

// afterLastDot returns the segment after the final '.', or the whole string if
// none — "users.v1.UserService" → "UserService", "gen/users/v1.UserServiceClient"
// → "UserServiceClient".
func afterLastDot(s string) string {
	if i := strings.LastIndex(s, "."); i >= 0 {
		return s[i+1:]
	}
	return s
}
