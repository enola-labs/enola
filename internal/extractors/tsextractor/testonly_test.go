package tsextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/facts"
	sitter "github.com/tree-sitter/go-tree-sitter"
	typescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"
)

// Entry points that only this package's tests call. They are kept out of the
// production files so the binary carries only what it runs.

// extractGraphQLCodeFirst reads root fields from the high-confidence code-first
// APIs used by NestJS/TypeGraphQL, Nexus, and Pothos. Package provenance and
// syntax are both required: a method called queryField, or an unrelated @Query
// decorator, must not become a GraphQL route merely because its spelling fits.
func extractGraphQLCodeFirst(src []byte, relFile string) []facts.Fact {
	lang := typescript.LanguageTypescript()
	if strings.HasSuffix(relFile, ".tsx") || strings.HasSuffix(relFile, ".jsx") {
		lang = typescript.LanguageTSX()
	}
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(lang)); err != nil {
		return nil
	}
	tree := parser.Parse(src, nil)
	defer tree.Close()
	kinds := tsKindsFor(strings.HasSuffix(relFile, ".tsx") || strings.HasSuffix(relFile, ".jsx"))
	root := tree.RootNode()
	return extractGraphQLCodeFirstAST(src, relFile, kinds, root, collectGraphQLImportBindings(kinds, root, src))
}

// extractGraphQLTagFacts pulls client operations out of a source file's tagged
// templates.
func extractGraphQLTagFacts(src []byte, relFile string) []facts.Fact {
	text := string(src)
	if !strings.Contains(text, "gql`") && !strings.Contains(text, "graphql`") {
		return nil
	}
	var out []facts.Fact
	for at := 0; at < len(text); {
		m := gqlTagOpen.FindStringIndex(text[at:])
		if m == nil {
			break
		}
		open := at + m[1] - 1
		body, end := templateBody(text, open)
		for _, f := range extractGraphQLClientOps(body, relFile, facts.RouteSourceGraphQLTag) {
			f.Line += strings.Count(text[:open+1], "\n")
			out = append(out, f)
		}
		at = end
	}
	return out
}

// extractGraphQLClientCallFacts covers clients that accept a document without
// a gql tag: graphql-request/urql request calls and plain fetch bodies with a
// `query` property. Only static string/template nodes containing an explicit
// operation are read. The package or fetch-body evidence is mandatory, keeping
// GraphQL examples in arbitrary strings inert.
func extractGraphQLClientCallFacts(src []byte, relFile string) []facts.Fact {
	text := string(src)
	possibleGraphQLRequest := strings.Contains(text, "graphql-request")
	fetchBody := hasGraphQLFetchBodyCandidate(text)
	if !possibleGraphQLRequest && !fetchBody {
		return nil
	}

	lang := typescript.LanguageTypescript()
	if strings.HasSuffix(relFile, ".tsx") || strings.HasSuffix(relFile, ".jsx") {
		lang = typescript.LanguageTSX()
	}
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(lang)); err != nil {
		return nil
	}
	tree := parser.Parse(src, nil)
	defer tree.Close()
	kinds := tsKindsFor(strings.HasSuffix(relFile, ".tsx") || strings.HasSuffix(relFile, ".jsx"))
	root := tree.RootNode()
	return extractGraphQLClientCallFactsAST(src, relFile, kinds, root, fetchBody, collectGraphQLImportBindings(kinds, root, src))
}

func detectSvelte(repoPath string) bool {
	tsRoot, _ := findTSRoot(repoPath)
	return hasPkgDependency(tsRoot, "svelte") || (tsRoot != repoPath && hasPkgDependency(repoPath, "svelte"))
}
