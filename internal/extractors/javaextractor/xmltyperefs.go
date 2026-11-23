package javaextractor

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// XML configuration names classes the framework loads by that name: a MyBatis
// mapper's result and parameter types, a MyBatis Generator plugin's `type`. No
// Java code names them, so without this the class read as unreferenced.
//
// Only a fully-qualified name of a type this repository declares binds, through
// the same index the SPI service files use. A short name (a MyBatis type alias)
// is left alone: which type an alias means is configuration this reading does not
// follow, and a guess would bind same-named types.

// xmlTypeAttr matches the attributes whose value is a class name.
var xmlTypeAttr = regexp.MustCompile(`\b(?:type|resultType|parameterType|ofType)\s*=\s*"([^"]+)"`)

// xmlConfigFile reports a production XML resource. Test resources are test code.
func xmlConfigFile(relFile string) bool {
	return strings.HasSuffix(strings.ToLower(relFile), ".xml") &&
		strings.Contains("/"+relFile, "/src/main/resources/")
}

// extractXMLTypeRefs emits one KindFileRef per XML resource naming at least one
// repository type, with a RelNames edge to each.
func extractXMLTypeRefs(repoPath string, files []string, typeIndex map[string]string) []facts.Fact {
	var out []facts.Fact
	for _, relFile := range files {
		if !xmlConfigFile(relFile) {
			continue
		}
		src, err := os.ReadFile(filepath.Join(repoPath, relFile))
		if err != nil {
			continue
		}
		seen := map[string]bool{}
		for _, m := range xmlTypeAttr.FindAllStringSubmatch(string(src), -1) {
			// A nested class is written Outer$Inner, the name the class loader takes.
			if canon, ok := typeIndex[strings.ReplaceAll(strings.TrimSpace(m[1]), "$", ".")]; ok {
				seen[canon] = true
			}
		}
		if len(seen) == 0 {
			continue
		}
		targets := make([]string, 0, len(seen))
		for t := range seen {
			targets = append(targets, t)
		}
		sort.Strings(targets)
		rels := make([]facts.Relation, 0, len(targets))
		for _, t := range targets {
			rels = append(rels, facts.Relation{Kind: facts.RelNames, Target: t})
		}
		out = append(out, facts.Fact{
			Kind:      facts.KindFileRef,
			Name:      relFile,
			File:      relFile,
			Line:      1,
			Props:     map[string]any{"language": "java"},
			Relations: rels,
		})
	}
	return out
}
