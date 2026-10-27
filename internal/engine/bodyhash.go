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
func (e *Engine) hashSymbolBodies(repoPath string, startIdx int) {
	var (
		file   string
		src    []byte
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
			files[file] = true
		}
		if h := facts.BodyHash(src, f.Line, f.EndLine); h != "" {
			f.SetProp(facts.PropBodyHash, h)
			hashed++
		}
	})
	if hashed > 0 {
		log.Printf("[engine] versioned %d symbol bodies in %d file(s)", hashed, len(files))
	}
}
