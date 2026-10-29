package rubyextractor

import (
	"github.com/enola-labs/enola/internal/facts"
)

// Entry points that only this package's tests call. They are kept out of the
// production files so the binary carries only what it runs.

// extractRubyHTTPClientFacts emits a client-route fact for every hand-written
// HTTP-client call (Faraday / Net::HTTP / wrapper clients) in a Ruby source
// file. These represent outbound calls the app makes, so the cross-repo linker
// can match them (by method + path suffix) to the service route that serves
// them. Paths are emitted as written; the linker's suffix matching reconciles
// base-path/prefix differences.
func extractRubyHTTPClientFacts(src []byte, relFile string) []facts.Fact {
	out, _, _ := extractRubyHTTPClientFactsCounted(src, relFile)
	return out
}

// parseRouteFileAST parses a Rails route file with tree-sitter and emits KindRoute
// facts. Block boundaries come from the grammar (do_block) rather than counting
// `do`/`end`, so nested namespaces/resources/scopes are tracked precisely.
func parseRouteFileAST(src []byte, relFile string) []facts.Fact {
	ff, _ := parseRouteFile(src, relFile, "", "", jsonapiContext{format: jsonapiRouteDasherized})
	return ff
}
