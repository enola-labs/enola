package server

import (
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/resolve"
)

// Name resolution lives in internal/resolve; these forward to it so call sites stay unchanged.

type (
	nameResolution  = resolve.NameResolution
	scoredCandidate = resolve.ScoredCandidate
)

// A server with no engine resolves against the store alone.
func (s *Server) resolver(store *facts.Store) resolve.Resolver {
	r := resolve.Resolver{Store: store}
	if s.eng == nil {
		return r
	}
	r.RepoPaths = s.eng.RepoPaths()
	if snap := s.eng.Snapshot(); snap != nil {
		r.RepoPath = snap.Meta.RepoPath
	}
	return r
}

func (s *Server) resolveNodeName(store *facts.Store, input string) (string, *nameResolution, error) {
	return s.resolver(store).NodeName(input)
}

func (s *Server) rankedCandidatesFor(store *facts.Store, input string) []scoredCandidate {
	return s.resolver(store).RankedCandidates(input)
}

// Both ask the facts which labels and file shapes the loaded graph has, so they
// need the store even though they resolve no name.
func (s *Server) maybePrefixRepoLabel(input string) string {
	return s.resolver(s.loadedStore()).PrefixRepoLabel(input)
}

func (s *Server) normalizeToRelative(p string) string {
	return s.resolver(s.loadedStore()).NormalizeToRelative(p)
}

func (s *Server) loadedStore() *facts.Store {
	if s.eng == nil {
		return nil
	}
	return s.eng.Store()
}

func (s *Server) repoLabels() []string {
	return s.resolver(nil).RepoLabels()
}

func (s *Server) expandFilePrefix(prefix string) []string {
	if s.eng == nil {
		return []string{prefix}
	}
	return s.resolver(s.eng.Store()).ExpandFilePrefix(prefix)
}
