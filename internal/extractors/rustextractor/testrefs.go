package rustextractor

import (
	"context"
	"log"
	"os"
	"path/filepath"

	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/parallel"
)

// ExtractTestRefs implements plugin.TestRefExtractor. It walks the Rust files the
// test globs set aside (Cargo integration tests and benches under tests/ and
// benches/, and tests/ module directories under src/) for the SOLE purpose of
// capturing their references into production code: one facts.KindTestRef fact per
// file, carrying only RelCalls edges.
//
// Each file is walked exactly as Extract walks a file a parent declares as
// `#[cfg(test)] mod name;`: in test mode, so it emits no symbol and every call it
// makes goes to the file's test reference. That keeps the two routes to test code
// resolving alike, which matters because a `#[cfg(test)] mod tests;` directory under
// src/ used to reach that walk through Extract and now reaches it through here.
//
// Resolution needs the crate index and the module layout. prodFiles supplies the
// Cargo manifests and the production modules; the test files join the module sets
// because a test resolves its own `mod common;` against them, as it did when Extract
// walked it.
func (e *RustExtractor) ExtractTestRefs(ctx context.Context, repoPath string, testFiles, prodFiles []string) ([]facts.Fact, error) {
	var rsFiles []string
	for _, relFile := range testFiles {
		if isRustFile(relFile) {
			rsFiles = append(rsFiles, relFile)
		}
	}
	if len(rsFiles) == 0 {
		return nil, nil
	}

	var cargoFiles []string
	moduleDirs := make(map[string]bool)
	moduleFiles := make(map[string]bool)
	addModule := func(relFile string) {
		moduleDirs[factpath.Dir(relFile)] = true
		moduleFiles[factpath.Slash(relFile)] = true
	}
	for _, relFile := range prodFiles {
		switch {
		case isRustFile(relFile):
			addModule(relFile)
		case filepath.Base(relFile) == "Cargo.toml":
			cargoFiles = append(cargoFiles, relFile)
		}
	}
	for _, relFile := range rsFiles {
		addModule(relFile)
	}
	crates := buildCrateIndex(repoPath, cargoFiles)

	perFile := parallel.MapFiles(ctx, rsFiles, func(relFile string) []facts.Fact {
		src, err := os.ReadFile(filepath.Join(repoPath, relFile))
		if err != nil {
			log.Printf("[rust-extractor] error reading test file %s: %v", relFile, err)
			return nil
		}
		var refs []facts.Fact
		for _, f := range extractRustFile(src, relFile, crates, moduleDirs, moduleFiles, true).facts {
			if f.Kind == facts.KindTestRef {
				refs = append(refs, f)
			}
		}
		return refs
	})

	var out []facts.Fact
	for _, ff := range perFile {
		out = append(out, ff...)
	}
	return out, nil
}
