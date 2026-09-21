package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/clientspec"
	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/facts"
)

// parseErrorSampleCap bounds the number of parse errors retained on the receipt.
const parseErrorSampleCap = 20

// hashPrefix is prepended to every hash value surfaced in the receipt
// (snapshot_id, config/glob hashes, output hashes), matching the notation issue
// #60 requested and making the algorithm self-describing.
const hashPrefix = "sha256:"

// sha256Prefixed returns the "sha256:"-prefixed hex digest of b — the canonical
// form for every hash value the receipt exposes.
func sha256Prefixed(b []byte) string {
	sum := sha256.Sum256(b)
	return hashPrefix + hex.EncodeToString(sum[:])
}

// computeIgnoreGlobHash returns a stable hash over the sorted ignore and test
// globs, so a receipt reveals when the exclusion set changed between two runs —
// the difference that would otherwise silently shift what got parsed.
func computeIgnoreGlobHash(cfg *config.Config) string {
	globs := make([]string, 0, len(cfg.Ignore)+len(cfg.TestGlobs))
	globs = append(globs, cfg.Ignore...)
	globs = append(globs, cfg.TestGlobs...)
	sort.Strings(globs)
	var sb strings.Builder
	for _, g := range globs {
		sb.WriteString(g)
		sb.WriteByte('\n')
	}
	return sha256Prefixed([]byte(sb.String()))
}

// computeConfigHash returns a stable hash over the effective configuration that
// governs a snapshot — the enabled extractors, explainers, renderers, the ignore
// and test globs, and the output settings. It is a superset of the ignore-glob
// hash: two snapshots with the same config_hash were produced under identical
// analysis settings, so a differing config_hash explains churn that is a config
// change rather than a code change. Each list is sorted so ordering never affects
// the hash.
func computeConfigHash(cfg *config.Config) string {
	var sb strings.Builder
	writeSortedSection := func(label string, vals []string) {
		sb.WriteString(label)
		sb.WriteByte(':')
		cp := append([]string(nil), vals...)
		sort.Strings(cp)
		for _, v := range cp {
			sb.WriteByte('\n')
			sb.WriteString(v)
		}
		sb.WriteByte('\n')
	}
	writeSortedSection("extractors", cfg.Extractors)
	writeSortedSection("explainers", cfg.Explainers)
	writeSortedSection("renderers", cfg.Renderers)
	writeSortedSection("ignore", cfg.Ignore)
	writeSortedSection("test_globs", cfg.TestGlobs)
	// Providers contribute facts, so the configured set — command and pinned
	// version included — is part of what a snapshot was generated over.
	provLines := make([]string, 0, len(cfg.Providers))
	for _, p := range cfg.Providers {
		provLines = append(provLines, p.Name+"\x00"+strings.Join(p.Command, "\x00")+"\x00"+p.ExpectedVersion)
	}
	writeSortedSection("providers", provLines)
	fmt.Fprintf(&sb, "output_dir:%s\nmax_context_tokens:%d\nincremental:%t\n",
		cfg.Output.Dir, cfg.Output.MaxContextTokens, cfg.IncrementalEnabled())
	// The linking vocabulary changes which cross-repo edges are drawn, so it changes
	// emitted facts. Without it here, two snapshots taken under different vocabularies
	// would carry the same config hash and compare as equivalent — and diff_snapshot
	// would report the resulting edge changes as a delta somebody made.
	//
	// An invalid overlay contributes its error text: the snapshot is already wrong in
	// that case, and hashing the failure keeps it from colliding with a valid one.
	if v, err := cfg.LinkingVocab(); err == nil {
		sb.WriteString("linking:\n")
		sb.WriteString(v.Fingerprint())
	} else {
		sb.WriteString("linking:invalid:" + err.Error() + "\n")
	}
	// Client specs decide which call sites become client routes, and service aliases
	// decide which repository such a route links to, so both change emitted facts.
	// Written only when declared, so a config declaring neither hashes exactly as before.
	if fp := clientspec.Fingerprint(cfg.Clients); fp != "" {
		sb.WriteString("clients:\n" + fp)
	}
	if fp := clientspec.AliasFingerprint(cfg.ServiceAliases); fp != "" {
		sb.WriteString("service_aliases:\n" + fp)
	}
	return sha256Prefixed([]byte(sb.String()))
}

// GitState reports the repository's current VCS state, or nil when it cannot be
// determined. Exported so a caller can ask "has the tree moved since this snapshot?"
// without taking a snapshot to find out — the session-start hook uses it to skip a pin
// that would reproduce the baseline it already has.
func GitState(repoPath, outputDir string) *facts.GitInfo {
	return gitInfo(repoPath, outputDir)
}

// gitInfo captures the repository's VCS state. It returns nil when repoPath is
// not a git working tree or git is unavailable, so a non-git or sandboxed run
// degrades to no git section rather than erroring.
//
// outputDir is the engine's output directory (cfg.Output.Dir), excluded from the
// dirtiness check. enola writes its own artifacts there and --porcelain lists
// untracked paths, so without the exclusion any repo that does not gitignore that
// directory records dirty:true from its first snapshot onward — the cache save
// creates it before this function runs, and it pre-exists every later run. That
// wrong baseline silently disables the git-drift half of the staleness check, whose
// "newly dirty" arm can only fire when the recorded state was clean (see
// freshness.go). The walker already excludes the same directory from analysis, so
// keeping it out here makes the flag describe the SOURCE rather than enola's own
// output. Pass "" to check the whole tree.
func gitInfo(repoPath, outputDir string) *facts.GitInfo {
	commit, err := runGit(repoPath, "rev-parse", "HEAD")
	if err != nil || commit == "" {
		return nil
	}
	info := &facts.GitInfo{Commit: commit}
	if ref, err := runGit(repoPath, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		info.Ref = ref
	}
	// The repository's identity, as opposed to this revision's. Recorded so a baseline
	// pinned under one checkout path can be recognized as the same repository under
	// another — the property that lets a CI baseline artifact be restored and diffed.
	// Absent origin, no remotes at all, or a detached/bare setup all just leave it empty,
	// matching how the rest of this function degrades.
	if remote, err := runGit(repoPath, "remote", "get-url", "origin"); err == nil {
		info.Remote = facts.NormalizeRemote(remote)
	}
	// --porcelain prints one line per changed/untracked path; any output => dirty.
	// The ':(exclude)' pathspec drops our own output dir; '.' is the positive
	// pathspec it subtracts from. An outputDir configured outside the repo simply
	// matches nothing, which is harmless.
	statusArgs := []string{"status", "--porcelain"}
	if outputDir != "" {
		statusArgs = append(statusArgs, "--", ".", ":(exclude)"+outputDir)
	}
	if status, err := runGit(repoPath, statusArgs...); err == nil && strings.TrimSpace(status) != "" {
		info.Dirty = true
	}
	return info
}

// runGit runs a git subcommand in repoPath and returns its trimmed stdout.
func runGit(repoPath string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", repoPath}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// coverageSummary rolls up the per-service edge_coverage counts the cross-repo
// linker records into a single snapshot-level summary. It returns nil for a
// single-repo snapshot (no service nodes), matching the coverage explainer.
//
// A gap is counted through facts.ClassifyService, so the receipt, coverage_report
// and the coverage explainer cannot drift apart. UnresolvedEdges accumulates for
// every service regardless of class — a partially-covered service still has blind
// spots worth reporting, it just is not a gap.
func coverageSummary(store *facts.Store) *facts.CoverageSummary {
	services := store.ByKind(facts.KindService)
	extractions := store.ByKind(facts.KindExtraction)
	if len(services) == 0 && len(extractions) == 0 {
		return nil
	}
	sum := &facts.CoverageSummary{ServicesTotal: len(services)}

	// An extractor accounts for its own blind spots in the same edge_coverage
	// shape, and the tallies stay separate on purpose: "this macro declares
	// routes I cannot read" and "this call site names a service I cannot find"
	// are different claims, and one number answering both answers neither.
	for _, extraction := range extractions {
		sum.ExtractorsReporting++
		sum.ExtractionUnresolved += readCoverageField(extraction, "unresolved")
	}
	if len(services) == 0 {
		return sum
	}
	for _, svc := range services {
		detected := readCoverageField(svc, "detected")
		unresolved := readCoverageField(svc, "unresolved")
		sum.UnresolvedEdges += unresolved
		sum.ExternalEdges += readCoverageField(svc, "external")
		if facts.ClassifyService(facts.DependsOnCount(svc), detected, unresolved) == facts.ServiceCoverageGap {
			sum.CoverageGaps++
		}
	}
	return sum
}

// readCoverageField sums one numeric field (e.g. "unresolved" or "external") across
// a service node's edge_coverage entries, tolerating both the in-memory shape and
// the float64 shape that survives a facts.jsonl JSON round-trip (mirrors
// coverage.readCoverage).
func readCoverageField(svc facts.Fact, field string) int {
	if svc.Props == nil {
		return 0
	}
	var raw []map[string]any
	switch v := svc.PropAny("edge_coverage").(type) {
	case []map[string]any:
		raw = v
	case []any:
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				raw = append(raw, m)
			}
		}
	default:
		return 0
	}
	total := 0
	for _, m := range raw {
		switch n := m[field].(type) {
		case int:
			total += n
		case float64:
			total += int(n)
		}
	}
	return total
}

// The snapshot ID is a stable content fingerprint of "what the graph was deterministic
// over": the byte-stable fact serialization, plus the version and the effective-config
// hash that gate what was parsed. It is deliberately NOT random — rerunning on the same
// inputs yields the same ID, so it can key equivalence. The result carries the
// "sha256:" prefix like every receipt hash.
//
// It is computed in two steps because the serialization is STREAMED into the hash.
// Buffering it first was the only reason GenerateSnapshot materialized the whole
// thing — a 792 MiB allocation on a large graph, built solely to be hashed and thrown
// away. The digest is the same either way: the same bytes reach the hash in the same
// order.

// newSnapshotIDHasher returns the hash to write the fact JSONL into. Pair with
// finishSnapshotID.
func newSnapshotIDHasher() hash.Hash { return sha256.New() }

// finishSnapshotID closes out a streamed snapshot ID with the trailing fields that are
// not part of the serialization.
func finishSnapshotID(h hash.Hash, enolaVersion, configHash string) string {
	h.Write([]byte(enolaVersion))
	h.Write([]byte(configHash))
	return hashPrefix + hex.EncodeToString(h.Sum(nil))
}

// capParseErrors trims a parse-error slice to the sample cap for the receipt.
func capParseErrors(errs []facts.ParseError) []facts.ParseError {
	if len(errs) <= parseErrorSampleCap {
		return errs
	}
	return errs[:parseErrorSampleCap]
}

// countHeuristicInsights returns how many insights are heuristics (confidence
// below 1.0) rather than structural facts (confidence 1.0). A high heuristic
// share is a signal about how much of the analysis is candidate-grade vs. certain.
func countHeuristicInsights(insights []facts.Insight) int {
	n := 0
	for _, in := range insights {
		if in.Confidence < 1.0 {
			n++
		}
	}
	return n
}

// repoLabelFor names the repository whose facts this snapshot is about.
//
// A remote identifies a repository ROOT and nothing below it, so it may only name the
// label when the indexed directory IS that root. Skipping the check labelled every
// sub-directory of a git repository with the parent's name — and a cluster assembled
// from directories inside one repository (a monorepo's apps, this repository's own
// cross-repo example) collapsed into a single label, merging two graphs into one, since
// graph nodes are name-keyed. That is a worse failure than the directory-derived label
// this replaced, so the remote is used only where it is actually an identity.
func repoLabelFor(absRepo, remote string) string {
	if remote != "" && isGitTopLevel(absRepo) {
		return facts.RepoLabel(remote, absRepo)
	}
	return filepath.Base(absRepo)
}

// isGitTopLevel reports whether absRepo is the root of its git repository (a clone or a
// linked worktree, both of which have their own root). Symlinks are resolved on both
// sides: on macOS a temporary directory is reached as /var/... and reported by git as
// /private/var/..., and comparing the two raw would answer "not the root" everywhere.
func isGitTopLevel(absRepo string) bool {
	top, err := runGit(absRepo, "rev-parse", "--show-toplevel")
	if err != nil || top == "" {
		return false
	}
	return samePath(top, absRepo)
}

func samePath(a, b string) bool {
	if a == b {
		return true
	}
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		return false
	}
	rb, err := filepath.EvalSymlinks(b)
	if err != nil {
		return false
	}
	return ra == rb
}
