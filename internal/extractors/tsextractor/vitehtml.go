package tsextractor

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"
	typescript "github.com/tree-sitter/tree-sitter-typescript/bindings/go"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
)

// viteHTMLFiles returns the .html files Vite serves as entry points: those at or
// below a directory holding a vite.config.*, outside test trees, each with the
// directory of its config (the root a `/src/main.ts` is resolved against).
func viteHTMLFiles(files []string) map[string]string {
	var roots []string
	for _, f := range files {
		base := path.Base(filepath.ToSlash(f))
		for _, name := range viteConfigNames {
			if base == name {
				roots = append(roots, factpath.Dir(filepath.ToSlash(f)))
			}
		}
	}
	if len(roots) == 0 {
		return nil
	}
	// The deepest root wins, as with tsconfig roots.
	sort.Slice(roots, func(i, j int) bool { return len(roots[i]) > len(roots[j]) })
	out := map[string]string{}
	for _, f := range files {
		f = filepath.ToSlash(f)
		if !isAngularTemplateFile(f) || facts.IsTestPath(f) {
			continue
		}
		dir := factpath.Dir(f)
		for _, r := range roots {
			if r == "" || r == "." || dir == r || strings.HasPrefix(dir, r+"/") {
				out[f] = r
				break
			}
		}
	}
	return out
}

// extractViteHTML reads the module scripts of a Vite HTML entry: an inline
// <script type="module"> contributes its imports, and a `src` naming a file in the
// repository is a dependency of the page on that file. Classic scripts cannot
// import, and a `src` naming no repository file is not guessed at.
func (e *TSExtractor) extractViteHTML(src []byte, relFile, viteRoot string, aliases map[string]tsAlias, files *tsFileIndex, knownFiles map[string]bool) []facts.Fact {
	var result []facts.Fact
	dir := factpath.Dir(relFile)
	for _, block := range extractVueScriptBlocks(src) {
		if !strings.EqualFold(extractAttr(block.Attrs, "type"), "module") {
			continue
		}
		if spec := extractAttr(block.Attrs, "src"); spec != "" {
			target := ""
			switch {
			case strings.HasPrefix(spec, "//") || strings.Contains(spec, "://"):
			case strings.HasPrefix(spec, "/"):
				target = factpath.Clean(path.Join(viteRoot, spec[1:]))
			default:
				target = factpath.Clean(path.Join(dir, spec))
			}
			if target == "" || !knownFiles[target] {
				continue
			}
			result = append(result, facts.Fact{
				Kind:      facts.KindDependency,
				Name:      dir + " -> " + target,
				File:      relFile,
				Line:      block.StartLine + 1,
				Props:     map[string]any{"language": "typescript", "source": "internal", "html_entry": true},
				Relations: []facts.Relation{{Kind: facts.RelImports, Target: target}},
			})
			continue
		}
		result = append(result, e.htmlModuleImports(block, relFile, aliases, files, knownFiles)...)
	}
	return mergeFileRefs(result)
}

// mergeFileRefs folds the file_refs of one file into the first, so a page with
// several inline module scripts is one node whatever its block count.
func mergeFileRefs(ff []facts.Fact) []facts.Fact {
	first := map[string]int{}
	out := ff[:0]
	for _, f := range ff {
		if f.Kind != facts.KindFileRef {
			out = append(out, f)
			continue
		}
		i, ok := first[f.Name]
		if !ok {
			first[f.Name] = len(out)
			out = append(out, f)
			continue
		}
		have := map[string]bool{}
		for _, r := range out[i].Relations {
			have[r.Kind+"\x00"+r.Target] = true
		}
		for _, r := range f.Relations {
			if !have[r.Kind+"\x00"+r.Target] {
				out[i].Relations = append(out[i].Relations, r)
			}
		}
	}
	return out
}

// htmlModuleImports reads one inline module script: its imports are the page's
// dependencies, and what it calls through them is the page's file_ref. The page
// declares no symbol, so without the file_ref nothing pointed at it and impact on a
// module it imports never listed the page.
func (e *TSExtractor) htmlModuleImports(block *vueScriptBlock, relFile string, aliases map[string]tsAlias, files *tsFileIndex, knownFiles map[string]bool) []facts.Fact {
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(typescript.LanguageTypescript())); err != nil {
		return nil
	}
	tree := parser.Parse(block.Content, nil)
	defer tree.Close()
	kinds := tsKindsFor(false)
	deps := e.extractImports(kinds, tree.RootNode(), block.Content, relFile, aliases, false, files)
	for i := range deps {
		deps[i].Line += block.StartLine
	}
	ctx := &extractCtx{src: block.Content, relFile: relFile, dir: factpath.Dir(relFile), knownFiles: knownFiles, files: files}
	refs := e.collectTSFileRefs(kinds, tree.RootNode(), ctx, aliases, facts.KindFileRef)
	// Only what the script imports. A bare call resolves to a declaration of the same
	// module, which is right in a source file and wrong here: the function a page's
	// script declares is no symbol, and the same-named one in a file beside the page
	// is not what it calls.
	named, internal := buildImportBindings(kinds, tree.RootNode(), block.Content, relFile, aliases, files)
	imported := map[string]bool{}
	for _, m := range []map[string]string{named, internal} {
		for _, t := range m {
			imported[t] = true
		}
	}
	for i := range refs {
		kept := refs[i].Relations[:0]
		for _, r := range refs[i].Relations {
			if imported[r.Target] || namespaceMember(r.Target, imported) {
				kept = append(kept, r)
			}
		}
		refs[i].Relations = kept
	}
	if len(refs) == 1 && len(refs[0].Relations) == 0 {
		refs = nil
	}
	return append(deps, refs...)
}

// namespaceMember reports whether target is a member of an imported binding
// (`import * as api from './api'`; `api.get()` names `<dir>.get` through `<dir>.api`).
func namespaceMember(target string, imported map[string]bool) bool {
	if i := strings.LastIndexByte(target, '.'); i > 0 {
		return imported[target[:i]]
	}
	return false
}

// viteHTMLFacts runs extractViteHTML over every Vite HTML entry in the repository.
func (e *TSExtractor) viteHTMLFacts(ctx context.Context, repoPath string, files []string, aliasRoots []tsAliasRoot, rc *repoCtx) []facts.Fact {
	entries := viteHTMLFiles(files)
	names := make([]string, 0, len(entries))
	for f := range entries {
		names = append(names, f)
	}
	sort.Strings(names)
	var result []facts.Fact
	for _, f := range names {
		if ctx.Err() != nil {
			return result
		}
		src, err := os.ReadFile(filepath.Join(repoPath, f)) //factpath:host
		if err != nil || !strings.Contains(strings.ToLower(string(src)), "type=\"module\"") && !strings.Contains(strings.ToLower(string(src)), "type='module'") {
			continue
		}
		result = append(result, e.extractViteHTML(src, f, entries[f], aliasesForDir(aliasRoots, factpath.Dir(f)), rc.files, rc.knownFiles)...)
	}
	return result
}
