package swiftextractor

import (
	"io"
	"os"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
)

// Entry points that only this package's tests call. They are kept out of the
// production files so the binary carries only what it runs.

// extractFileAST parses a Swift file with tree-sitter and emits architectural
// facts. It is a superset of the legacy regex extractor: every declaration /
// import / iOS-classification fact is preserved, and call-graph relations
// (RelCalls / RelInstantiates / RelInjects) plus View->ViewModel RelDependsOn
// edges are attached to symbol facts when call sites, initializer parameters or
// dependency-injection property wrappers are observed.
//
// Edge targets are emitted as bare simple type names here (e.g. "AppComposition")
// and canonicalised to "<dir>.<Type>" by the post-pass in Extract, which has the
// project-wide type index needed to resolve them.
func extractFileAST(src []byte, relFile string, isiOS bool) []facts.Fact {
	return extractFileASTWithDir(src, relFile, isiOS, factpath.Dir(relFile))
}

// extractFileASTWithDir is extractFileAST with an explicit module identity dir.
// Extract passes the file's resolved target module (from moduleResolver) so
// symbols are named "<targetDir>.<Type>" and declare into the target module
// rather than the file's leaf directory.
func extractFileASTWithDir(src []byte, relFile string, isiOS bool, dir string) []facts.Fact {
	var out []facts.Fact
	withTree(src, relFile, isiOS, dir, func(_ *sitter.Node, walked []facts.Fact) {
		out = walked
	})
	return out
}

// extractURLSessionFacts emits a client-route fact for every URLSession request
// in a Swift source file. The path comes from baseURL.appendingPathComponent("…")
// and the HTTP method from a nearby `.httpMethod = "…"` assignment (defaulting
// to GET when none is found within methodWindow lines). Paths are base-relative
// (no /api prefix); the cross-repo linker's suffix matching reconciles that
// against the backend's full path.
func extractURLSessionFacts(src []byte, relFile string) []facts.Fact {
	return extractURLSessionFactsWithDir(src, relFile, factpath.Dir(relFile))
}

// extractFile reads a Swift file and delegates to the tree-sitter walker. It
// preserves the legacy signature used by the test helper extractFromString.
func extractFile(f *os.File, relFile string, isiOS bool) []facts.Fact {
	src, err := io.ReadAll(f)
	if err != nil {
		return nil
	}
	return extractFileAST(src, relFile, isiOS)
}
