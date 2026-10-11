package tsextractor

import (
	"path/filepath"
	"strings"

	"github.com/enola-labs/enola/internal/extractors/detectnames"
)

// plainJSExts are the extensions of JavaScript source that names no framework and
// needs no build: what a browser page or a small Node program is written in.
var plainJSExts = map[string]bool{".js": true, ".jsx": true, ".mjs": true, ".cjs": true}

// notOwnJSDirs are directories whose JavaScript is someone else's (a checked-in
// library) or the output of a build, and so says nothing about what the repository
// is written in. One Scala web application in the benchmark corpus carries 524
// JavaScript files under assets/vendors and four of its own.
var notOwnJSDirs = []string{
	"node_modules", "bower_components", "vendor", "vendors", "third_party", "third-party",
	"dist", "build", "out", "coverage",
}

// otherSourceExts maps the extensions of other programming languages to the language,
// so that files of one language are counted together (.c with .h, .kt with .kts). It
// lists languages no extractor reads as well: a repository written in one of those is
// still not a JavaScript project for carrying a few scripts.
var otherSourceExts = map[string]string{
	".go": "go", ".py": "python", ".rb": "ruby", ".php": "php", ".rs": "rust",
	".java": "java", ".kt": "kotlin", ".kts": "kotlin", ".scala": "scala", ".groovy": "groovy",
	".swift": "swift", ".m": "objc", ".mm": "objc", ".dart": "dart",
	".cs": "csharp", ".fs": "fsharp", ".vb": "vb",
	".c": "c", ".h": "c", ".cc": "cpp", ".cpp": "cpp", ".cxx": "cpp", ".hpp": "cpp",
	".ex": "elixir", ".exs": "elixir", ".erl": "erlang", ".hs": "haskell", ".ml": "ocaml",
	".clj": "clojure", ".lua": "lua", ".pl": "perl", ".pm": "perl", ".r": "r", ".jl": "julia",
	".zig": "zig", ".nim": "nim", ".cr": "crystal", ".sh": "shell", ".bash": "shell", ".ps1": "powershell",
}

// javaScriptLeads reports whether the repository is written in plain JavaScript: it has
// JavaScript of its own, and more files of it than of any other one language.
//
// Detecting on a .js file alone would make a JavaScript project of nearly every
// repository, since a Go service or a Rails app carries a build script or a page asset.
// Requiring JavaScript to lead keeps those out, and lets in the project the markers in
// hasTSMarkers cannot see: one with no tsconfig, no framework dependency, and often no
// package.json at all, which until now was snapshotted as its README and nothing else.
func javaScriptLeads(files []string) bool {
	js := 0
	others := map[string]int{}
	for _, rel := range files {
		if detectnames.HasAnySegment(rel, notOwnJSDirs...) {
			continue
		}
		ext := strings.ToLower(filepath.Ext(rel))
		switch {
		case plainJSExts[ext]:
			if !strings.HasSuffix(strings.ToLower(rel), ".min.js") {
				js++
			}
		case otherSourceExts[ext] != "":
			others[otherSourceExts[ext]]++
		}
	}
	if js == 0 {
		return false
	}
	for _, n := range others {
		if n >= js {
			return false
		}
	}
	return true
}
