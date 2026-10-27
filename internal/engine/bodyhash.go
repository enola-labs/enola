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
func (e *Engine) hashSymbolBodies(repoPath string, startIdx int) {
	var (
		file   string
		src    []byte
		starts []int
		files  = map[string]bool{}
		hashed int
	)
	e.store.UpdateRange(startIdx, func(f *facts.Fact) {
		if f.Kind != facts.KindSymbol || f.File == "" || f.Line <= 0 || f.EndLine < f.Line {
			return
		}
		if f.File != file {
			file = f.File
			src, _ = os.ReadFile(filepath.Join(repoPath, file))
			starts = lineStarts(src)
			files[file] = true
		}
		if f.Line > len(starts) {
			return
		}
		if h := facts.BodyHash(src[starts[f.Line-1]:], 1, f.EndLine-f.Line+1); h != "" {
			f.SetProp(facts.PropBodyHash, h)
			hashed++
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
