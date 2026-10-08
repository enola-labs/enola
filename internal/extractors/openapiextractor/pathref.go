package openapiextractor

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
	"gopkg.in/yaml.v3"
)

// maxPathItemRefHops bounds a chain of path-item references; a cycle or a
// chain longer than this derives nothing rather than looping.
const maxPathItemRefHops = 5

// pathItemResolver follows `$ref` path items for one spec file. A split spec
// keeps each path's operations in its own file:
//
//	paths:
//	  /item/{item_id}:
//	    $ref: './paths/item.yml#/paths/~1item~1{item_id}'
//	  /api/v1/users/{user_id}/exports:
//	    $ref: './paths/users_{user_id}_exports.yaml'
//
// Read without the reference the path has no operations, and the service's
// whole API extracts as nothing.
type pathItemResolver struct {
	specAbs  string
	repoRoot string
	docs     map[string]any // parsed referenced files, by absolute path
}

func newPathItemResolver(specAbs, relFile string) *pathItemResolver {
	root := strings.TrimSuffix(filepath.Clean(specAbs), filepath.FromSlash(relFile))                   //factpath:host
	return &pathItemResolver{specAbs: specAbs, repoRoot: filepath.Clean(root), docs: map[string]any{}} //factpath:host
}

// resolve returns the path item a `$ref` points at, and the repository-relative
// file it was read from. An item that is not a reference is returned as is,
// with an empty file. A reference that cannot be followed — remote, outside
// the repository, missing, or into a file that is itself a full OpenAPI spec
// (and so extracts its own routes) — yields the original item, which carries
// no operations and so derives nothing.
func (r *pathItemResolver) resolve(item map[string]any) (map[string]any, string) {
	current, file := item, ""
	base := r.specAbs
	for hop := 0; hop < maxPathItemRefHops; hop++ {
		ref, ok := current["$ref"].(string)
		if !ok || ref == "" {
			return current, file
		}
		target, targetAbs, ok := r.follow(base, ref)
		if !ok {
			return item, ""
		}
		current, base = target, targetAbs
		if rel, err := filepath.Rel(r.repoRoot, targetAbs); err == nil {
			file = factpath.Slash(rel)
		}
	}
	return item, ""
}

// follow reads one reference relative to the file holding it: `file`,
// `file#/pointer` or `#/pointer`.
func (r *pathItemResolver) follow(base, ref string) (map[string]any, string, bool) {
	filePart, pointer, _ := strings.Cut(ref, "#")
	if strings.Contains(filePart, "://") {
		return nil, "", false
	}
	targetAbs := base
	if filePart != "" {
		targetAbs = filepath.Clean(filepath.Join(filepath.Dir(base), filepath.FromSlash(filePart))) //factpath:host
	}
	if rel, err := filepath.Rel(r.repoRoot, targetAbs); err != nil || strings.HasPrefix(rel, "..") { //factpath:host
		return nil, "", false
	}
	doc, ok := r.load(targetAbs)
	if !ok {
		return nil, "", false
	}
	// A full spec elsewhere already extracts its own routes; resolving into it
	// would emit each of them twice.
	if targetAbs != r.specAbs {
		if m, isMap := doc.(map[string]any); isMap && (m["openapi"] != nil || m["swagger"] != nil) {
			return nil, "", false
		}
	}
	node, ok := jsonPointer(doc, pointer)
	if !ok {
		return nil, "", false
	}
	m, ok := node.(map[string]any)
	return m, targetAbs, ok
}

func (r *pathItemResolver) load(abs string) (any, bool) {
	if doc, ok := r.docs[abs]; ok {
		return doc, doc != nil
	}
	data, err := os.ReadFile(abs)
	var doc any
	if err == nil {
		if yaml.Unmarshal(data, &doc) != nil {
			doc = nil
		}
	}
	r.docs[abs] = doc
	return doc, doc != nil
}

// jsonPointer walks an RFC 6901 pointer (`/paths/~1item~1{item_id}`) through
// decoded YAML maps. An empty pointer is the document itself.
func jsonPointer(doc any, pointer string) (any, bool) {
	if pointer == "" || pointer == "/" {
		return doc, true
	}
	node := doc
	for _, tok := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		m, ok := node.(map[string]any)
		if !ok {
			return nil, false
		}
		if node, ok = m[tok]; !ok {
			return nil, false
		}
	}
	return node, true
}
