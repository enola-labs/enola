package facts

import "strings"

// kindPlural spells out the kinds that appending "s" gets wrong: "dependencys" and
// "storages".
var kindPlural = map[string]string{
	KindStorage:    "storage",
	KindDependency: "dependencies",
}

// PluralKind is a fact kind as a count of more than one reads: "symbols",
// "dependencies", "storage", "test_refs".
func PluralKind(kind string) string {
	if p, ok := kindPlural[kind]; ok {
		return p
	}
	if strings.HasSuffix(kind, "s") {
		return kind
	}
	return kind + "s"
}
