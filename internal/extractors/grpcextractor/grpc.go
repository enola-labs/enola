// Package grpcextractor extracts architectural facts from Protocol Buffers
// (.proto) service definitions. Each RPC becomes a server-role KindRoute fact
// whose Name is the gRPC wire path "/pkg.Service/Method" — the same shape a
// gRPC-web client hits over HTTP POST — so proto services flow through the
// existing cross-repo route linker and the unused-routes explainer exactly like
// HTTP endpoints, letting enola report which RPCs no loaded client calls.
//
// Services and RPCs are also emitted as symbol facts (interface / method) and
// messages as struct symbols, so proto participates in traverse/impact_analysis.
package grpcextractor

import (
	"context"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/workspace"
)

// GRPCExtractor parses .proto files and emits gRPC route/symbol facts.
type GRPCExtractor struct{}

// New creates a new GRPCExtractor.
func New() *GRPCExtractor {
	return &GRPCExtractor{}
}

// Name returns the extractor identifier.
func (e *GRPCExtractor) Name() string { return "grpc" }

// OwnsFile reports whether this extractor parses the given file, so the engine
// can scope its incremental cache to .proto changes.
func (e *GRPCExtractor) OwnsFile(relFile string) bool {
	return strings.HasSuffix(relFile, ".proto")
}

// Detect returns true if the repository contains any .proto file. It walks the
// tree (bounded by the same directory skips other extractors use) because Detect
// runs before the engine hands over the file list.
func (e *GRPCExtractor) Detect(repoPath string) (bool, error) {
	found := false
	err := filepath.WalkDir(repoPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil || found {
			return err //nolint:wrapcheck // propagate walk error verbatim
		}
		if d.IsDir() {
			if skipDir(d.Name()) || (path != repoPath && workspace.IsLinkedWorktree(path)) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), ".proto") {
			found = true
		}
		return nil
	})
	return found, err
}

// Extract parses every .proto file in the walker-provided file list and emits
// facts. .proto files are not excluded by the default ignore globs, so the
// engine passes them in `files` and no independent walk is needed here.
func (e *GRPCExtractor) Extract(ctx context.Context, repoPath string, files []string) ([]facts.Fact, error) {
	var out []facts.Fact
	seenModule := map[string]bool{}

	for _, rel := range files {
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		default:
		}
		if !strings.HasSuffix(rel, ".proto") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(repoPath, rel))
		if err != nil {
			log.Printf("[grpc-extractor] error reading %s: %v", rel, err)
			continue
		}
		pf := scanProto(src)
		dir := factpath.Dir(rel)

		if !seenModule[dir] {
			seenModule[dir] = true
			out = append(out, facts.Fact{
				Kind: facts.KindModule,
				Name: dir,
				File: dir,
				Props: map[string]any{
					"language": "grpc",
					"package":  pf.pkg,
				},
			})
		}

		out = append(out, factsForProtoFile(pf, rel, dir)...)
	}
	return out, nil
}

// factsForProtoFile turns one parsed .proto into its facts: a server route per
// RPC, a symbol per service/RPC/message, and a dependency per import.
func factsForProtoFile(pf protoFile, relFile, dir string) []facts.Fact {
	var out []facts.Fact

	for _, imp := range pf.imports {
		out = append(out, facts.Fact{
			Kind: facts.KindDependency,
			Name: imp,
			File: relFile,
			Props: map[string]any{
				"language": "grpc",
				"external": isWellKnownImport(imp),
			},
			Relations: []facts.Relation{{Kind: facts.RelImports, Target: dir}},
		})
	}

	for _, msg := range pf.messages {
		out = append(out, facts.Fact{
			Kind: facts.KindSymbol,
			Name: dir + "." + msg.name,
			File: relFile,
			Line: msg.line,
			Props: map[string]any{
				"symbol_kind": symbolKindForMessage(msg.kind),
				"language":    "grpc",
				"exported":    true,
			},
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: dir}},
		})
	}

	for _, svc := range pf.services {
		fqService := qualifiedService(pf.pkg, svc.name)
		serviceSym := dir + "." + svc.name

		out = append(out, facts.Fact{
			Kind: facts.KindSymbol,
			Name: serviceSym,
			File: relFile,
			Line: svc.line,
			Props: map[string]any{
				"symbol_kind":       facts.SymbolInterface,
				"language":          "grpc",
				facts.PropFramework: facts.FrameworkGRPC,
				"exported":          true,
				"rpc_service":       fqService,
			},
			Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: dir}},
		})

		for _, rpc := range svc.rpcs {
			path := "/" + fqService + "/" + rpc.name

			// The RPC as a graph symbol (a method of the service interface).
			out = append(out, facts.Fact{
				Kind: facts.KindSymbol,
				Name: serviceSym + "." + rpc.name,
				File: relFile,
				Line: rpc.line,
				Props: map[string]any{
					"symbol_kind":       facts.SymbolMethod,
					"language":          "grpc",
					facts.PropFramework: facts.FrameworkGRPC,
					"exported":          true,
					"streaming":         streamingKind(rpc),
				},
				Relations: []facts.Relation{
					{Kind: facts.RelDeclares, Target: dir},
					{Kind: facts.RelHasMethod, Target: serviceSym},
				},
			})

			// The served endpoint (role omitted => treated as server by the linker).
			out = append(out, facts.Fact{
				Kind: facts.KindRoute,
				Name: path,
				File: relFile,
				Line: rpc.line,
				Props: map[string]any{
					"method":            "POST",
					facts.PropRole:      facts.RoleServer,
					facts.PropFramework: facts.FrameworkGRPC,
					"language":          "grpc",
					facts.PropSource:    facts.RouteSourceGRPCProto,
					"type":              "grpc",
					"rpc_service":       fqService,
					"rpc_method":        rpc.name,
					"streaming":         streamingKind(rpc),
				},
				Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: dir}},
			})
		}
	}

	return out
}

// qualifiedService joins the proto package and service name into the fully
// qualified name used in the gRPC wire path ("users.v1.UserService"). A proto
// with no package declaration yields the bare service name.
func qualifiedService(pkg, service string) string {
	if pkg == "" {
		return service
	}
	return pkg + "." + service
}

// streamingKind classifies an RPC's streaming shape for the `streaming` prop.
func streamingKind(rpc protoRPC) string {
	switch {
	case rpc.clientStream && rpc.serverStream:
		return "bidi"
	case rpc.clientStream:
		return "client"
	case rpc.serverStream:
		return "server"
	default:
		return "none"
	}
}

func symbolKindForMessage(kind string) string {
	if kind == "enum" {
		return facts.SymbolEnum
	}
	return facts.SymbolStruct
}

// isWellKnownImport reports whether a proto import path is a Google/protobuf
// well-known type rather than an in-repo definition, so it is tagged external.
func isWellKnownImport(path string) bool {
	return strings.HasPrefix(path, "google/protobuf/") ||
		strings.HasPrefix(path, "google/api/") ||
		strings.HasPrefix(path, "grpc/")
}

// skipDir matches directories that never contain first-party .proto worth
// indexing (VCS, deps, build output, enola artifacts, Go fixture data).
//
// Only Detect consults this — Extract consumes the engine's file list, which is
// already ignore-glob filtered. Detect runs BEFORE that list exists and so must
// repeat the exclusions itself, or a repo whose only .proto sit under testdata
// enables the gRPC extractor on the strength of fixtures it does not serve.
func skipDir(name string) bool {
	switch name {
	case ".git", "node_modules", "vendor", ".enola", "dist", "build", ".next", "testdata":
		return true
	}
	return false
}
