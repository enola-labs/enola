package engine

import (
	"log"
	"os"
	"path/filepath"

	"github.com/enola-labs/enola/internal/facts"
)

// hashSymbolBodies sets facts.PropBodyHash on every symbol from this run whose
// extractor measured a span.
//
// It runs after extraction rather than inside each extractor, so every span an
// extractor measures gets a version without that extractor knowing about it, and a
// symbol served from the extractor cache gets one exactly as a freshly parsed one
// does. Only files holding a spanned symbol are read, so a language with no spans
// costs nothing here. An extractor emits a file's facts together, so one file is
// held at a time; a file revisited later is read again rather than kept. It runs
// before the paths are repo-prefixed, while File still resolves against repoPath.
//
// A file's line starts are indexed once, so each symbol hashes its own span instead of
// scanning from the top of the file: on a file with thousands of methods the scan was
// quadratic, and on a large repository it made a warm snapshot 70% slower.
//
// A symbol whose id it shares with another also gets facts.PropDeclHash, so a reader can
// tell which of them it means once their bodies have changed. Repo is not set yet, but
// this range is one repository, so name and file are the whole identity here.
func (e *Engine) hashSymbolBodies(repoPath string, startIdx int) {
	var (
		file   string
		src    []byte
		starts []int
		files  = map[string]bool{}
		hashed int
	)
	spanned := func(f *facts.Fact) bool {
		return f.Kind == facts.KindSymbol && f.File != "" && f.Line > 0 && f.EndLine >= f.Line
	}
	type identity struct{ name, file string }
	declarations := map[identity]int{}
	e.store.UpdateRange(startIdx, func(f *facts.Fact) {
		if spanned(f) {
			declarations[identity{f.Name, f.File}]++
		}
	})
	e.store.UpdateRange(startIdx, func(f *facts.Fact) {
		if !spanned(f) {
			return
		}
		if f.File != file {
			file = f.File
			src, _ = os.ReadFile(filepath.Join(repoPath, file))
			starts = lineStarts(src)
			files[file] = true
		}
		if first := f.SpanStart(); first > 0 && first <= len(starts) {
			if h := facts.BodyHash(src[starts[first-1]:], 1, f.EndLine-first+1); h != "" {
				f.SetProp(facts.PropBodyHash, h)
				hashed++
			}
		}
		if f.Line <= len(starts) && declarations[identity{f.Name, f.File}] > 1 {
			if h := facts.DeclHash(src[starts[f.Line-1]:], 1, f.EndLine-f.Line+1); h != "" {
				f.SetProp(facts.PropDeclHash, h)
			}
		}
	})
	if hashed > 0 {
		log.Printf("[engine] versioned %d symbol bodies in %d file(s)", hashed, len(files))
	}
}

// lineStarts returns the byte offset at which each line of src begins.
func lineStarts(src []byte) []int {
	starts := []int{0}
	for i, b := range src {
		if b == '\n' {
			starts = append(starts, i+1)
		}
	}
	return starts
}
