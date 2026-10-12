package phpextractor

import (
	"encoding/json"
	"github.com/enola-labs/enola/internal/factpath"
	"os"
	"path/filepath"
	"strings"
)

// phpFramework names the web framework a PHP repository uses, which selects the
// server-route extraction pass. HTTP-client detection runs regardless of framework.
type phpFramework string

const (
	frameworkPlain     phpFramework = "plain"
	frameworkWordPress phpFramework = "wordpress"
	frameworkLaravel   phpFramework = "laravel"
	frameworkSymfony   phpFramework = "symfony"
)

// detectPHPFramework classifies a PHP repository so the right route DSL is parsed.
// WordPress is checked first (it has the most specific markers), then Laravel and
// Symfony via composer dependencies and characteristic files. A repo with no
// recognized framework is frameworkPlain (symbols/calls/clients only, no routes).
func detectPHPFramework(repoPath string) phpFramework {
	if detectWordPress(repoPath) {
		return frameworkWordPress
	}
	req := composerRequires(repoPath)
	switch {
	case hasComposerDep(req, "laravel/framework") || hasComposerDep(req, "laravel/lumen-framework") ||
		fileExists(repoPath, "artisan") ||
		fileExists(repoPath, "routes/web.php") || fileExists(repoPath, "routes/api.php"):
		return frameworkLaravel
	case hasComposerDep(req, "symfony/framework-bundle") || hasComposerDep(req, "symfony/symfony") ||
		(fileExists(repoPath, "bin/console") && fileExists(repoPath, "config")):
		return frameworkSymfony
	}
	return frameworkPlain
}

// composerRequires returns the merged require + require-dev map (package -> version
// constraint) from a repo's composer.json, or an empty map when absent/unparseable.
func composerRequires(repoPath string) map[string]string {
	data, err := os.ReadFile(filepath.Join(repoPath, "composer.json"))
	if err != nil {
		return nil
	}
	var doc struct {
		Require    map[string]string `json:"require"`
		RequireDev map[string]string `json:"require-dev"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil
	}
	out := make(map[string]string, len(doc.Require)+len(doc.RequireDev))
	for k, v := range doc.Require {
		out[strings.ToLower(k)] = v
	}
	for k, v := range doc.RequireDev {
		out[strings.ToLower(k)] = v
	}
	return out
}

// hasComposerDep reports whether pkg is present in a composer requires map
// (case-insensitive).
func hasComposerDep(req map[string]string, pkg string) bool {
	_, ok := req[strings.ToLower(pkg)]
	return ok
}

// fileExists reports whether rel exists (file or dir) under repoPath.
func fileExists(repoPath, rel string) bool {
	_, err := os.Stat(filepath.Join(repoPath, rel))
	return err == nil
}

// isLaravelRouteFile reports whether relFile lives under a routes directory, where
// the Route::… DSL lives. The file names are the application's to choose: web.php
// and api.php are the skeleton's, and an application splits them (api.base.php,
// routes/api/v1.php) or keeps a routes directory per module. A file there that
// registers nothing yields nothing.
func isLaravelRouteFile(relFile string) bool {
	for _, seg := range strings.Split(factpath.Dir(relFile), "/") {
		if seg == "routes" {
			return true
		}
	}
	return false
}
