package dartextractor

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/json"
	"sort"
	"strings"
	"sync"

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
	defer zr.Close()
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

// withSDKIndex appends the bundled SDK signatures for the libraries the facts
// import, unless the repository carries its own SDK sources. The appended facts
// are resolver input only; callers drop them before returning.
func withSDKIndex(all []facts.Fact) ([]facts.Fact, int) {
	idx := loadDartSDKIndex()
	n := len(all)
	used := map[string]bool{idx.Libs["dart:core"]: true}
	files := map[string]bool{}
	for i := range all {
		f := &all[i]
		switch f.Kind {
		case facts.KindDependency:
			uri := f.PropString("uri")
			if !strings.HasPrefix(uri, "dart:") {
				continue
			}
			if f.PropString("resolved_file") != "" {
				return all, n
			}
			if host, ok := idx.Libs[uri]; ok {
				used[host] = true
			}
		case facts.KindSymbol:
			files[f.File] = true
		}
	}
	if len(files) == 0 || idx.Libs["dart:core"] == "" {
		return all, n
	}
	names := make([]string, 0, len(files))
	for file := range files {
		names = append(names, file)
	}
	sort.Strings(names)
	for _, file := range names {
		all = append(all, facts.Fact{Kind: facts.KindDependency, Name: file + " -> dart:core", File: file, Props: map[string]any{"uri": "dart:core", "implicit": true}})
	}
	for _, f := range idx.Facts {
		if used[f.File] {
			all = append(all, f)
		}
	}
	return all, n
}

// sdkIndexHost is the bundled host file of a dart: URI.
func sdkIndexHost(uri string) string { return loadDartSDKIndex().Libs[uri] }
