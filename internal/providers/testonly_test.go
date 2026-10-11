package providers

import (
	"context"

	"github.com/enola-labs/enola/internal/facts"
)

// Entry points that only this package's tests call. They are kept out of the
// production files so the binary carries only what it runs.

// Run executes every configured provider against repoPath and returns the
// merged, stamped, sorted facts plus one census record per provider — the
// receipt's account of who contributed what, including the providers that
// contributed nothing and why. taken reports whether an extractor already owns
// a kind+name identity; colliding provider facts are skipped, never merged.
// ignored reports whether a repo-relative file is excluded by the repository's
// ignore globs, and a fact about such a file is dropped: a provider walks the
// tree itself, so it cannot know what the configuration excludes, and a
// vendored dependency the extractors never read must not enter the graph
// through the seam instead. Run never fails the snapshot: every per-provider
// failure mode is a named skip in the census.
func Run(ctx context.Context, providers []Provider, repoPath string, taken func(kind, name string) bool, ignored func(file string) bool) ([]facts.Fact, []facts.ProviderRecord) {
	return RunWith(ctx, Input{Providers: providers, RepoPath: repoPath, Taken: taken, Ignored: ignored})
}
