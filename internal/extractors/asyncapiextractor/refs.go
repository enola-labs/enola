package asyncapiextractor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/enola-labs/enola/internal/workspace"
	"gopkg.in/yaml.v3"
)

// refResolver resolves local JSON References across YAML/JSON documents. Reads
// are confined to the checkout at repoRoot: a contract cannot use ../../ or an
// absolute path to read files outside the repository, nor reach into a linked
// worktree nested in it.
type refResolver struct {
	repoRoot string
	docs     map[string]map[string]any
}

type resolvedMap struct {
	value   map[string]any
	absFile string
	ref     string
}

func newRefResolver(repoRoot, rootFile string, root map[string]any) *refResolver {
	rootFile = filepath.Clean(rootFile) //factpath:host
	return &refResolver{
		repoRoot: filepath.Clean(repoRoot), //factpath:host
		docs:     map[string]map[string]any{rootFile: root},
	}
}

func (r *refResolver) resolve(raw any, baseFile string) resolvedMap {
	current := resolvedMap{value: mapValue(raw), absFile: filepath.Clean(baseFile)} //factpath:host
	seen := map[string]bool{}
	for len(current.value) > 0 {
		ref := stringValue(current.value["$ref"])
		if ref == "" {
			return current
		}
		key := current.absFile + "\x00" + ref
		if seen[key] {
			return resolvedMap{}
		}
		seen[key] = true
		next, ok := r.follow(ref, current.absFile)
		if !ok {
			return resolvedMap{}
		}
		if current.ref == "" {
			current.ref = ref
		}
		current.value, current.absFile = next.value, next.absFile
	}
	return current
}

func (r *refResolver) follow(ref, baseFile string) (resolvedMap, bool) {
	if strings.Contains(ref, "://") || strings.HasPrefix(ref, "//") {
		return resolvedMap{}, false
	}
	filePart, fragment, _ := strings.Cut(ref, "#")
	targetFile := filepath.Clean(baseFile) //factpath:host
	if filePart != "" {
		if filepath.IsAbs(filePart) {
			return resolvedMap{}, false
		}
		targetFile = filepath.Clean(filepath.Join(filepath.Dir(baseFile), filepath.FromSlash(filePart))) //factpath:host
	}
	// Outside the repository, or inside a linked worktree nested in it: not this
	// checkout's file, though the second is under the root and on disk.
	if !workspace.InCheckout(r.repoRoot, targetFile) {
		return resolvedMap{}, false
	}
	doc, err := r.load(targetFile)
	if err != nil {
		return resolvedMap{}, false
	}
	var value any = doc
	if fragment != "" {
		value, err = jsonPointer(doc, fragment)
		if err != nil {
			return resolvedMap{}, false
		}
	}
	return resolvedMap{value: mapValue(value), absFile: targetFile, ref: ref}, true
}

func (r *refResolver) load(path string) (map[string]any, error) {
	path = filepath.Clean(path) //factpath:host
	if doc, ok := r.docs[path]; ok {
		return doc, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	r.docs[path] = doc
	return doc, nil
}

func jsonPointer(root any, pointer string) (any, error) {
	if pointer == "" {
		return root, nil
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, fmt.Errorf("unsupported fragment %q", pointer)
	}
	current := root
	for _, encoded := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		part := strings.ReplaceAll(strings.ReplaceAll(encoded, "~1", "/"), "~0", "~")
		m, ok := current.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("pointer segment %q is not an object", part)
		}
		current, ok = m[part]
		if !ok {
			return nil, fmt.Errorf("pointer segment %q not found", part)
		}
	}
	return current, nil
}
