package dartextractor

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/json"
	"sort"
	"sync"

	"github.com/enola-labs/enola/internal/factpath"

	"github.com/enola-labs/enola/internal/facts"
)

//go:embed sdk_index.json.gz
var dartSDKIndexData []byte

type dartSDKIndex struct {
	Libs  map[string]string `json:"libs"`
	Facts []facts.Fact      `json:"facts"`
}

var loadDartSDKIndex = sync.OnceValue(func() *dartSDKIndex {
	idx := &dartSDKIndex{}
	zr, err := gzip.NewReader(bytes.NewReader(dartSDKIndexData))
	if err != nil {
		return idx
	}
	defer func() { _ = zr.Close() }()
	if json.NewDecoder(zr).Decode(idx) != nil {
		return &dartSDKIndex{}
	}
	for i := range idx.Facts {
		for key, value := range idx.Facts[i].Props {
			if list, ok := value.([]any); ok {
				strs := make([]string, 0, len(list))
				for _, item := range list {
					if s, ok := item.(string); ok {
						strs = append(strs, s)
					}
				}
				idx.Facts[i].Props[key] = strs
			}
		}
	}
	return idx
})

// withSDKIndex appends the bundled SDK and Flutter signatures for the libraries
// the facts import, skipping any library whose sources the repository carries.
// The appended facts are resolver input only; callers drop them before returning.
func withSDKIndex(all []facts.Fact) ([]facts.Fact, int) {
	idx := loadDartSDKIndex()
	n := len(all)
	hostOf := map[string]string{}
	for uri, host := range idx.Libs {
		hostOf[host] = uri
	}
	present := map[string]bool{}
	files := map[string]bool{}
	for i := range all {
		f := &all[i]
		if f.Kind == facts.KindSymbol {
			files[f.File] = true
		}
		for dir := factpath.Dir(f.File); dir != "" && dir != "." && !present[dir]; dir = factpath.Dir(dir) {
			present[dir] = true
		}
	}
	if len(files) == 0 || idx.Libs["dart:core"] == "" {
		return all, n
	}
	local := func(host string) bool { return present[factpath.Dir(host)] }
	used := map[string]bool{}
	var mark func(host string)
	mark = func(host string) {
		if used[host] || local(host) {
			return
		}
		used[host] = true
		for _, f := range idx.Facts {
			if f.Kind == facts.KindDependency && f.File == host {
				if next := idx.Libs[f.PropString("uri")]; next != "" {
					mark(next)
				}
			}
		}
	}
	mark(idx.Libs["dart:core"])
	for i := range all {
		if f := &all[i]; f.Kind == facts.KindDependency {
			if host := idx.Libs[f.PropString("uri")]; host != "" && f.PropString("resolved_file") == "" {
				mark(host)
			}
		}
	}
	if used[idx.Libs["dart:core"]] {
		names := make([]string, 0, len(files))
		for file := range files {
			names = append(names, file)
		}
		sort.Strings(names)
		for _, file := range names {
			all = append(all, facts.Fact{Kind: facts.KindDependency, Name: file + " -> dart:core", File: file, Props: map[string]any{"uri": "dart:core", "implicit": true}})
		}
	}
	for _, f := range idx.Facts {
		if used[f.File] {
			all = append(all, f)
		}
	}
	return all, n
}

// sdkIndexHost is the bundled host file of a dart: or package: URI.
func sdkIndexHost(uri string) string { return loadDartSDKIndex().Libs[uri] }
