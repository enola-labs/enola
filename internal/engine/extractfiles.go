package engine

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/pathglob"
	"github.com/enola-labs/enola/pkg/plugin"
)

// ExtractFiles returns the facts the extractors emit for a few files of the tree at root,
// labeled repoLabel and with their symbols versioned, and nothing else: no links, no
// explainers, no artifacts, no cache.
//
// A snapshot's extractor cache is keyed per extractor, so one changed file re-extracts its
// whole language. Walking history that way costs a full extraction per commit however small
// the commit is, and a question about which symbols a commit changed needs only the files
// it changed. What those facts lack is what needs the rest of the tree: relations resolved
// across files, and anything linked. Names, kinds and spans are read from the file itself.
//
// root must be the whole tree, since an extractor reads its configuration from it. A file a
// snapshot would ignore stays ignored. Nothing here walks the tree, which on a repository
// with many directories costs more than the parsing: an extractor runs because it owns one
// of the files, so detection has nothing to add.
func (e *Engine) ExtractFiles(ctx context.Context, root, repoLabel string, files []string) ([]facts.Fact, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolving %s: %w", root, err)
	}
	ignored := e.ignoredPaths(absRoot)
	var keep []string
	for _, f := range files {
		if !ignored(f) {
			keep = append(keep, f)
		}
	}

	store := facts.NewStore()
	for _, ext := range e.extractors.All() {
		owner, ok := ext.(plugin.FileOwner)
		if !ok || !e.cfg.IsExtractorEnabled(ext.Name()) {
			continue
		}
		var owned []string
		for _, f := range keep {
			if owner.OwnsFile(f) {
				owned = append(owned, f)
			}
		}
		if len(owned) == 0 {
			continue
		}
		extracted, err := ext.Extract(ctx, absRoot, owned)
		if err != nil {
			var fatal *plugin.FatalError
			if errors.As(err, &fatal) {
				return nil, fmt.Errorf("extractor %s: %w", ext.Name(), fatal.Err)
			}
			log.Printf("[engine] extractor %s error: %v", ext.Name(), err)
			continue
		}
		store.Add(extracted...)
	}
	saved := e.store
	e.store = store
	defer func() { e.store = saved }()
	e.hashSymbolBodies(absRoot, 0)
	e.store.SetRepoRange(0, repoLabel)
	return e.store.FactsRef(), nil
}

// ignoredPaths answers what walkRepo's ignore check answers for a file, without the walk:
// a directory it would have pruned hides the file, so each parent is asked too.
func (e *Engine) ignoredPaths(root string) func(string) bool {
	ignoreSet := pathglob.Compile(e.cfg.Ignore)
	pkgs := newPyPackageExemption(root, e.cfg.Ignore)
	hit := func(p string, dir bool) bool {
		pattern, ok := ignoreSet.Match(p)
		return ok && !pkgs.exempt(p, pattern, dir)
	}
	return func(file string) bool {
		for i, c := range file {
			if c == '/' && hit(file[:i], true) {
				return true
			}
		}
		return hit(file, false)
	}
}

// RepoLabel is the label a snapshot of the repository at path would give its facts, so
// that ids computed outside a snapshot match the ones inside it.
func RepoLabel(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	var remote string
	if git := gitInfo(abs, ""); git != nil {
		remote = git.Remote
	}
	return repoLabelFor(abs, remote), nil
}
