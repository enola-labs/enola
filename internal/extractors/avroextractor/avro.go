// Package avroextractor reads Avro schemas: IDL protocols (.avdl) and JSON
// schemas (.avsc).
//
// Every named type, a record, error, enum or fixed, becomes a symbol named
// "avro.<namespace>.<name>", with a depends_on edge to each named type its fields
// use and a names edge to its bare full name. The full name is how Avro's record
// name strategy names a schema's subject, and how a topic carrying one record
// type is often named, so a topic of that name, and the code consuming it, meet
// the schema on one node.
package avroextractor

import (
	"context"
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/enola-labs/enola/internal/extractors/detectnames"
	"github.com/enola-labs/enola/internal/facts"
)

// Extractor reads Avro schema files.
type Extractor struct{}

// New creates the extractor.
func New() *Extractor { return &Extractor{} }

// Name returns the extractor name.
func (e *Extractor) Name() string { return "avro" }

func isAvro(rel string) bool {
	switch strings.ToLower(path.Ext(rel)) {
	case ".avdl", ".avsc":
		return true
	}
	return false
}

// OwnsFile scopes caching to schema files.
func (e *Extractor) OwnsFile(relFile string) bool { return isAvro(relFile) }

// Detect reports a schema file anywhere in the repository.
func (e *Extractor) Detect(repoPath string) (bool, error) {
	return e.DetectFiles(repoPath, detectnames.Walk(repoPath))
}

// DetectFiles implements plugin.FileListDetector.
func (e *Extractor) DetectFiles(_ string, files []string) (bool, error) {
	for _, rel := range files {
		if isAvro(rel) {
			return true, nil
		}
	}
	return false, nil
}

// namedType is one record, error, enum or fixed with what it references.
type namedType struct {
	namespace, name, kind, doc string
	line                       int
	refs                       []string // as written: bare or dotted
}

func (t namedType) fullName() string {
	if t.namespace == "" || strings.Contains(t.name, ".") {
		return t.name
	}
	return t.namespace + "." + t.name
}

// Extract emits a symbol per named type.
func (e *Extractor) Extract(ctx context.Context, repoPath string, files []string) ([]facts.Fact, error) {
	type found struct {
		file  string
		types []namedType
	}
	var all []found
	declared := map[string]bool{}
	for _, rel := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !isAvro(rel) {
			continue
		}
		src, err := os.ReadFile(filepath.Join(repoPath, rel))
		if err != nil {
			continue
		}
		var types []namedType
		if strings.EqualFold(path.Ext(rel), ".avsc") {
			types = parseJSONSchema(src)
		} else {
			types = parseIDL(string(src))
		}
		for _, t := range types {
			declared[t.fullName()] = true
		}
		all = append(all, found{rel, types})
	}

	var out []facts.Fact
	for _, fd := range all {
		for _, t := range fd.types {
			full := t.fullName()
			props := map[string]any{
				"symbol_kind":           facts.SymbolType,
				"language":              "avro",
				"avro_kind":             t.kind,
				"namespace":             t.namespace,
				facts.PropFrameworkRoot: "avro",
			}
			if t.doc != "" {
				facts.SetDocProp(props, t.doc)
			}
			f := facts.Fact{
				Kind: facts.KindSymbol, Name: "avro." + full, File: fd.file, Line: t.line, Props: props,
				Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: path.Dir(fd.file)}},
			}
			seen := map[string]bool{}
			for _, ref := range t.refs {
				target := resolve(ref, t.namespace, declared)
				if target == "" || target == full || seen[target] {
					continue
				}
				seen[target] = true
				f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelDependsOn, Target: "avro." + target})
			}
			if t.kind == "record" {
				f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelNames, Target: full})
			}
			out = append(out, f)
		}
	}
	return out, nil
}

// resolve names a referenced type: a dotted name as written, a bare one in the
// referring type's namespace first, then as written. A name no schema declares
// stays unresolved.
func resolve(ref, namespace string, declared map[string]bool) string {
	if !strings.Contains(ref, ".") && namespace != "" && declared[namespace+"."+ref] {
		return namespace + "." + ref
	}
	if declared[ref] {
		return ref
	}
	return ""
}

var avroPrimitives = map[string]bool{
	"null": true, "boolean": true, "int": true, "long": true, "float": true, "double": true,
	"bytes": true, "string": true, "union": true, "array": true, "map": true, "void": true,
	"date": true, "time_ms": true, "timestamp_ms": true, "local_timestamp_ms": true,
	"decimal": true, "uuid": true, "true": true, "false": true, "oneway": true, "throws": true,
	"record": true, "error": true, "enum": true, "fixed": true, "protocol": true, "import": true,
	"idl": true, "schema": true, "protocol_ref": true,
}

var (
	idlNamespace = regexp.MustCompile(`@namespace\(\s*"([^"]*)"\s*\)`)
	idlDecl      = regexp.MustCompile(`\b(record|error|enum|fixed)\s+([A-Za-z_][A-Za-z0-9_]*)`)
	idlTypeToken = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_.]*`)
)

// parseIDL reads the named types of an Avro IDL file. Comments, documentation
// and annotation arguments are set aside first; a field line's type is what
// precedes its name.
func parseIDL(src string) []namedType {
	clean, docs := stripIDL(src)
	protocolNS := ""
	if m := idlNamespace.FindStringSubmatchIndex(clean); m != nil {
		// The first @namespace, if it precedes the protocol keyword, is the protocol's.
		if p := strings.Index(clean, "protocol"); p < 0 || m[0] < p {
			protocolNS = clean[m[2]:m[3]]
		}
	}
	var out []namedType
	for _, m := range idlDecl.FindAllStringSubmatchIndex(clean, -1) {
		kind, name := clean[m[2]:m[3]], clean[m[4]:m[5]]
		t := namedType{namespace: protocolNS, name: name, kind: kind, line: 1 + strings.Count(clean[:m[0]], "\n")}
		// An @namespace written just before the declaration overrides the protocol's.
		head := clean[max(0, m[0]-200):m[0]]
		if a := idlNamespace.FindAllStringSubmatch(head, -1); len(a) > 0 && !strings.Contains(head[strings.LastIndex(head, "@namespace"):], ";") && !strings.Contains(head[strings.LastIndex(head, "@namespace"):], "}") {
			t.namespace = a[len(a)-1][1]
		}
		t.doc = docBefore(clean, docs, m[0])
		if kind == "record" || kind == "error" {
			open := strings.IndexByte(clean[m[1]:], '{')
			if open >= 0 {
				body := block(clean, m[1]+open)
				for _, stmt := range strings.Split(body, ";") {
					t.refs = append(t.refs, fieldTypeRefs(stmt)...)
				}
			}
		}
		out = append(out, t)
	}
	return out
}

// docBefore returns the documentation comment written above a declaration: the
// one ending nearest before it with nothing but blanks (annotations included,
// already blanked) in between.
func docBefore(clean string, docs map[int]string, at int) string {
	best, text := -1, ""
	for end, d := range docs {
		if end <= at && end > best && strings.TrimSpace(clean[end:at]) == "" {
			best, text = end, d
		}
	}
	return text
}

// fieldTypeRefs returns the named types a field declaration's type uses: every
// identifier before the field's name that is not a primitive or a keyword.
func fieldTypeRefs(stmt string) []string {
	stmt = strings.TrimSpace(stmt)
	if i := strings.IndexByte(stmt, '='); i >= 0 {
		stmt = stmt[:i] // the default value
	}
	toks := idlTypeToken.FindAllString(stmt, -1)
	if len(toks) < 2 {
		return nil
	}
	var out []string
	for _, tok := range toks[:len(toks)-1] { // the last token is the field's name
		if !avroPrimitives[tok] {
			out = append(out, tok)
		}
	}
	return out
}

// block returns the text between the brace at open and its match.
func block(s string, open int) string {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[open+1 : i]
			}
		}
	}
	return s[open+1:]
}

// stripIDL blanks comments and annotations (the arguments of all but
// @namespace), keeping newlines so offsets and lines hold, and returns each
// documentation comment keyed by the offset just past it.
func stripIDL(src string) (string, map[int]string) {
	b := []byte(src)
	docs := map[int]string{}
	line := 1
	for i := 0; i < len(b); i++ {
		switch {
		case b[i] == '\n':
			line++
		case b[i] == '/' && i+1 < len(b) && b[i+1] == '/':
			for i < len(b) && b[i] != '\n' {
				b[i] = ' '
				i++
			}
			i--
		case b[i] == '/' && i+1 < len(b) && b[i+1] == '*':
			start := i
			for i < len(b) && (b[i] != '*' || i+1 >= len(b) || b[i+1] != '/') {
				i++
			}
			end := min(i+2, len(b))
			text := string(src[start:end])
			for j := start; j < end; j++ {
				if b[j] == '\n' {
					line++
				} else {
					b[j] = ' '
				}
			}
			if strings.HasPrefix(text, "/**") {
				d := strings.TrimSuffix(strings.TrimPrefix(text, "/**"), "*/")
				var parts []string
				for _, l := range strings.Split(d, "\n") {
					if l = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "*")); l != "" {
						parts = append(parts, l)
					}
				}
				docs[end] = strings.Join(parts, " ")
			}
			i = end - 1
		case b[i] == '@':
			// @annotation(args): blank the arguments, which may hold JSON with
			// braces, keeping @namespace("…") readable.
			j := i + 1
			for j < len(b) && (b[j] == '-' || b[j] == '_' || (b[j] >= 'a' && b[j] <= 'z') || (b[j] >= 'A' && b[j] <= 'Z')) {
				j++
			}
			name := string(b[i+1 : j])
			if j < len(b) && b[j] == '(' && name != "namespace" {
				depth := 0
				for k := j; k < len(b); k++ {
					switch b[k] {
					case '(':
						depth++
					case ')':
						depth--
					}
					if b[k] == '\n' {
						line++
					} else {
						b[k] = ' '
					}
					if depth == 0 {
						j = k
						break
					}
				}
			}
			if name != "namespace" {
				for k := i; k < j && k < len(b); k++ {
					if b[k] != '\n' {
						b[k] = ' '
					}
				}
			}
			i = j
		}
	}
	return string(b), docs
}

// parseJSONSchema reads the named types of a .avsc file, nested ones included.
func parseJSONSchema(src []byte) []namedType {
	var v any
	if json.Unmarshal(src, &v) != nil {
		return nil
	}
	var out []namedType
	var walk func(v any, ns string) string
	walk = func(v any, ns string) string {
		switch x := v.(type) {
		case string:
			if !avroPrimitives[x] {
				return x
			}
		case []any:
			for _, e := range x {
				walk(e, ns)
			}
		case map[string]any:
			kind, _ := x["type"].(string)
			name, _ := x["name"].(string)
			switch kind {
			case "record", "error", "enum", "fixed":
				if n, ok := x["namespace"].(string); ok {
					ns = n
				}
				t := namedType{namespace: ns, name: name, kind: kind, line: 1}
				if d, ok := x["doc"].(string); ok {
					t.doc = d
				}
				if fields, ok := x["fields"].([]any); ok {
					for _, fl := range fields {
						if fm, ok := fl.(map[string]any); ok {
							t.refs = append(t.refs, typeRefs(fm["type"], ns, walk)...)
						}
					}
				}
				out = append(out, t)
			default:
				walk(x["type"], ns)
				walk(x["items"], ns)
				walk(x["values"], ns)
			}
		}
		return ""
	}
	walk(v, "")
	return out
}

// typeRefs returns the named types a field's type mentions, registering any type
// it defines inline along the way.
func typeRefs(v any, ns string, walk func(any, string) string) []string {
	switch x := v.(type) {
	case string:
		if !avroPrimitives[x] {
			return []string{x}
		}
	case []any:
		var out []string
		for _, e := range x {
			out = append(out, typeRefs(e, ns, walk)...)
		}
		return out
	case map[string]any:
		switch x["type"] {
		case "record", "error", "enum", "fixed":
			walk(x, ns)
			if n, ok := x["name"].(string); ok {
				return []string{n}
			}
		case "array":
			return typeRefs(x["items"], ns, walk)
		case "map":
			return typeRefs(x["values"], ns, walk)
		default:
			return typeRefs(x["type"], ns, walk)
		}
	}
	return nil
}
