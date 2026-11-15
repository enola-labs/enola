package dartextractor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/enola-labs/enola/internal/factpath"
)

// The SDK's manifest supplies library identities. Multiple platform URIs remain
// ambiguous outside their platform tree; public libraries normally share a URI.
func loadDartSDKLibraries(repo string) map[string][]string {
	out := map[string][]string{}
	for _, manifest := range []string{"sdk/lib/libraries.json", "lib/libraries.json"} {
		data, err := os.ReadFile(filepath.Join(repo, manifest))
		if err != nil {
			continue
		}
		var targets map[string]json.RawMessage
		if json.Unmarshal(data, &targets) != nil {
			continue
		}
		for _, rawTarget := range targets {
			var target struct {
				Libraries map[string]json.RawMessage `json:"libraries"`
			}
			if json.Unmarshal(rawTarget, &target) != nil {
				continue
			}
			for name, rawLibrary := range target.Libraries {
				var lib struct {
					URI string `json:"uri"`
				}
				if json.Unmarshal(rawLibrary, &lib) != nil || lib.URI == "" {
					continue
				}
				path := factpath.Join(factpath.Dir(manifest), lib.URI)
				key := "dart:" + name
				if !slices.Contains(out[key], path) {
					out[key] = append(out[key], path)
				}
			}
		}

	}
	for key := range out {
		slices.Sort(out[key])
	}
	return out
}
func (idx *packageIndex) resolveSDKURI(uri, file string) (string, bool) {
	candidates := idx.sdkLibraries[uri]
	if len(candidates) == 1 {
		return candidates[0], true
	}
	best, score, tied := "", 0, false
	fileParts := strings.Split(file, "/")
	for _, path := range candidates {
		parts := strings.Split(path, "/")
		common := 0
		for common < len(parts) && common < len(fileParts) && parts[common] == fileParts[common] {
			common++
		}
		if common > score {
			best, score, tied = path, common, false
		} else if common == score {
			tied = true
		}
	}
	// Platform selection requires shared _internal/<platform> ancestry.
	if !tied && score >= 4 && strings.Contains(best, "/_internal/") && strings.Contains(file, "/_internal/") {
		return best, true
	}
	return "", false
}
