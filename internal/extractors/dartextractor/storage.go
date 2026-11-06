package dartextractor

import (
	"strings"

	sitter "github.com/enola-labs/enola/internal/treesitter"

	"github.com/enola-labs/enola/internal/facts"
)

// Local and remote data stores a Dart package names.
//
// Every rule here is gated on the declaring package being imported, for the reason the
// Scala extractor documents at length: the marker words are ordinary vocabulary. `Table`
// is a widget in Flutter's own material library — `Table(children: [...])` lays out a
// grid — so `extends Table` means a drift database table only in a file that imports
// drift, and would otherwise turn every layout widget in an app into a storage fact.

// extractStorage emits storage facts for the declared data stores.
func (w *walker) extractStorage(root *sitter.Node) {
	if w.importsAny("package:drift/", "package:moor/") {
		w.driftTables(root)
	}
	if w.importsAny("package:isar/") {
		w.annotatedStores(root, "collection", "Collection", "isar", "collection")
	}
	if w.importsAny("package:hive/", "package:hive_flutter/") {
		w.annotatedStores(root, "HiveType", "", "hive", "model")
	}
	if w.importsAny("package:objectbox/") {
		w.annotatedStores(root, "Entity", "", "objectbox", "entity")
	}
	if w.importsAny("package:floor/") {
		w.annotatedStores(root, "Entity", "", "floor", "entity")
	}
	if w.importsAny("package:cloud_firestore/") {
		w.firestoreCollections(root)
	}
}

// driftTables reads `class Todos extends Table`.
//
// drift derives the SQL table name from the class name by lower-snake-casing it, and
// allows an override via a `tableName` getter. The derived name is what the database
// actually holds, so it is what the fact carries.
func (w *walker) driftTables(root *sitter.Node) {
	for _, n := range namedChildren(root) {
		if kindOf(n) != "class_definition" {
			continue
		}
		supers := supertypeNames(n, w.src)
		if !containsAny(supers, "Table") {
			continue
		}
		className := identifierChild(n, w.src)
		if className == "" {
			continue
		}
		tableName := snakeCase(className)
		if override := w.tableNameOverride(n); override != "" {
			tableName = override
		}
		w.out = append(w.out, facts.Fact{
			Kind: facts.KindStorage, Name: tableName, File: w.relFile, Line: lineOf(n),
			Props: map[string]any{
				"language": "dart", "storage_kind": "table",
				facts.PropFramework: "drift", "class": w.qualify(className),
			},
		})
	}
}

// tableNameOverride reads drift's `String get tableName => 'custom';`.
func (w *walker) tableNameOverride(class *sitter.Node) string {
	body := childOfKind(class, "class_body")
	if body == nil {
		return ""
	}
	kids := namedChildren(body)
	for i, c := range kids {
		if kindOf(c) != "method_signature" {
			continue
		}
		getter := firstOfKind(c, "getter_signature")
		if getter == nil || signatureName(getter, w.src) != "tableName" {
			continue
		}
		if body := nextBody(kids, i); body != nil {
			if lit := firstOfKind(body, "string_literal"); lit != nil {
				return stringLiteralValue(lit, w.src)
			}
		}
	}
	return ""
}

// annotatedStores emits a storage fact per class carrying a marker annotation.
func (w *walker) annotatedStores(root *sitter.Node, anno, altAnno, framework, kind string) {
	for _, n := range namedChildren(root) {
		if kindOf(n) != "class_definition" {
			continue
		}
		annos := annotationNames(n, w.src)
		if !containsAny(annos, anno) && (altAnno == "" || !containsAny(annos, altAnno)) {
			continue
		}
		className := identifierChild(n, w.src)
		if className == "" {
			continue
		}
		w.out = append(w.out, facts.Fact{
			Kind: facts.KindStorage, Name: snakeCase(className), File: w.relFile, Line: lineOf(n),
			Props: map[string]any{
				"language": "dart", "storage_kind": kind,
				facts.PropFramework: framework, "class": w.qualify(className),
			},
		})
	}
}

// firestoreCollections reads `.collection('users')` literals.
//
// Only a literal collection name is emitted: a computed one (`collection(path)`) names
// a store whose identity the extractor cannot know, and inventing one would put a
// collection in the graph that no code reads.
func (w *walker) firestoreCollections(root *sitter.Node) {
	seen := map[string]bool{}
	var visit func(*sitter.Node)
	visit = func(n *sitter.Node) {
		kids := namedChildren(n)
		for i, c := range kids {
			if kindOf(c) != "selector" || childOfKind(c, "argument_part") == nil {
				continue
			}
			name, _, _ := w.calleeOf(kids, i)
			if name != "collection" && name != "collectionGroup" {
				continue
			}
			coll := stringLiteralValue(positionalArg(argumentsOf(childOfKind(c, "argument_part")), 0), w.src)
			if coll == "" || seen[coll] {
				continue
			}
			seen[coll] = true
			w.out = append(w.out, facts.Fact{
				Kind: facts.KindStorage, Name: coll, File: w.relFile, Line: lineOf(c),
				Props: map[string]any{
					"language": "dart", "storage_kind": "collection",
					facts.PropFramework: "firestore",
				},
			})
		}
		for _, c := range kids {
			visit(c)
		}
	}
	visit(root)
}

func containsAny(haystack []string, needles ...string) bool {
	for _, h := range haystack {
		for _, n := range needles {
			if h == n {
				return true
			}
		}
	}
	return false
}

// snakeCase converts a Dart class name to the lower_snake_case name the ORMs derive.
func snakeCase(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteByte(c - 'A' + 'a')
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}
