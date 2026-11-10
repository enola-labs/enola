package goextractor

import (
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// dirNamed renames facts from import-path names back to the repository-directory
// names Go packages carried before they were named by import path: example.com/app/x
// becomes x, and the root package example.com/app becomes ".". The tests written
// against directory names keep testing what they were written for, resolution and
// edges, through it; TestPackagesAreNamedByImportPath and its neighbours test the
// naming itself, on the extractor's real output.
func dirNamed(t *testing.T, repo string, ff []facts.Fact) []facts.Fact {
	t.Helper()
	var files []string
	_ = filepath.WalkDir(repo, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && d.Name() == "go.mod" {
			rel, _ := filepath.Rel(repo, p)
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	mods := readModules(repo, files)
	back := func(s string) string {
		for _, mod := range mods.list { // longest module path first
			mp := mod.path
			switch {
			case s == mp:
				return mod.dir
			case strings.HasPrefix(s, mp+"/"):
				rest := s[len(mp)+1:]
				if mod.dir == "." {
					return rest
				}
				return mod.dir + "/" + rest
			case strings.HasPrefix(s, mp+"."):
				return mod.dir + s[len(mp):]
			}
		}
		return s
	}
	out := make([]facts.Fact, len(ff))
	for i, f := range ff {
		f.Name = back(f.Name)
		rels := make([]facts.Relation, len(f.Relations))
		for j, r := range f.Relations {
			r.Target = back(r.Target)
			rels[j] = r
		}
		f.Relations = rels
		if f.Props != nil {
			props := make(map[string]any, len(f.Props))
			for k, v := range f.Props {
				props[k] = backValue(v, back)
			}
			f.Props = props
		}
		out[i] = f
	}
	return out
}

func backValue(v any, back func(string) string) any {
	switch x := v.(type) {
	case string:
		return back(x)
	case []string:
		y := make([]string, len(x))
		for i, s := range x {
			y[i] = back(s)
		}
		return y
	case []any:
		y := make([]any, len(x))
		for i, s := range x {
			y[i] = backValue(s, back)
		}
		return y
	case map[string]string:
		y := make(map[string]string, len(x))
		for k, s := range x {
			y[back(k)] = back(s)
		}
		return y
	case map[string]any:
		y := make(map[string]any, len(x))
		for k, s := range x {
			y[back(k)] = backValue(s, back)
		}
		return y
	}
	return v
}
