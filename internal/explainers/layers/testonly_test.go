package layers

// Entry points that only this package's tests call. They are kept out of the
// production files so the binary carries only what it runs.

// matchesLayer checks if a module path contains any of the given patterns.
func matchesLayer(modulePath string, patterns []string) bool {
	return matchesLayerIn(modulePath, patterns, false)
}
