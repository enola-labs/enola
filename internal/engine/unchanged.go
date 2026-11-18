package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/internal/version"
)

// Unchanged is what CheckUnchanged found.
type Unchanged struct {
	// Same is true when the graph on disk is exactly what a generate would write now.
	Same bool
	// Reason says why it is not, naming the first repository and file that differ.
	Reason string
	// GeneratedAt is when the graph on disk was written; Dir is where it is.
	GeneratedAt, Dir string
	// Checked is how many repositories were compared, Read how many files had to be
	// read because their size or modification time did not settle it.
	Checked, Read int
}

// CheckUnchanged reports whether the graph of members already on disk is what a
// generate would write now, so the generate can be skipped. It holds when the graph's
// directory has a complete snapshot written by this enola release and extraction
// version under this config, for exactly these repositories, each still at the commit it was read at and
// with every file enola walks as it was: none added, none removed, none changed.
//
// A file whose size and modification time are the ones recorded when it was hashed is
// taken as unchanged without being read; any other file, and any file that had just
// been modified when it was hashed (recorded without a time), is read and its hash
// compared. git is asked only
// for each repository's commit: files git ignores are walked by enola and compared
// here, and an uncommitted change shows in the files.
//
// Anything that cannot be compared answers no, with the reason: the cost of a wrong
// yes is a graph that does not describe the tree.
func (e *Engine) CheckUnchanged(members []string) Unchanged {
	abs := make([]string, 0, len(members))
	for _, m := range members {
		a, err := filepath.Abs(m)
		if err != nil {
			return Unchanged{Reason: err.Error()}
		}
		abs = append(abs, a)
	}
	sort.Strings(abs)
	dir := e.cfg.GraphOutputDir(abs)
	u := Unchanged{Dir: dir, Checked: len(abs)}

	if _, err := os.Stat(filepath.Join(dir, "facts.jsonl")); err != nil {
		u.Reason = "no graph written yet"
		return u
	}
	var union facts.SnapshotMeta
	if raw, err := os.ReadFile(filepath.Join(dir, snapshotMetaFileName)); err != nil || json.Unmarshal(raw, &union) != nil {
		u.Reason = "the graph's meta cannot be read"
		return u
	}
	u.GeneratedAt = union.GeneratedAt
	switch {
	case union.ExtractorVersion != cacheVersion:
		u.Reason = fmt.Sprintf("written by extractor version %s, this build is %s", union.ExtractorVersion, cacheVersion)
		return u
	case union.EnolaVersion != version.Version:
		// A release can change what the same facts link into (a binder, a signal)
		// without an extractor change; an upgrade rebuilds the graph once.
		u.Reason = fmt.Sprintf("written by enola %s, this build is %s", union.EnolaVersion, version.Version)
		return u
	case union.ConfigHash != computeConfigHash(e.cfg):
		u.Reason = "written under another config"
		return u
	case union.IgnoreGlobHash != computeIgnoreGlobHash(e.cfg):
		u.Reason = "written under other ignore globs"
		return u
	}

	// Each repository's own record: members.json for a cluster, the meta for one.
	recorded := map[string]facts.SnapshotMeta{}
	if len(abs) > 1 {
		m, err := ReadMembersMeta(dir)
		if err != nil {
			u.Reason = "the graph records no members"
			return u
		}
		recorded = m
		if len(recorded) != len(abs) {
			u.Reason = fmt.Sprintf("the graph holds %d repositories, %d are configured", len(recorded), len(abs))
			return u
		}
	} else {
		recorded[union.RepoPath] = union
	}

	type result struct {
		reason string
		read   int
	}
	results := make([]result, len(abs))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < unchangedWorkers(len(abs)); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				meta, ok := recorded[abs[i]]
				if !ok {
					results[i] = result{reason: filepath.Base(abs[i]) + " is not in the graph"}
					continue
				}
				reason, read := e.memberChanged(abs[i], meta)
				results[i] = result{reason, read}
			}
		}()
	}
	for i := range abs {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	for _, r := range results {
		u.Read += r.read
		if r.reason != "" && u.Reason == "" {
			u.Reason = r.reason
		}
	}
	u.Same = u.Reason == ""
	return u
}

// unchangedWorkers is how many repositories are checked at once: the check reads
// directories and file metadata, so a few more than the cores keep the disk busy.
func unchangedWorkers(n int) int {
	w := 8
	if n < w {
		w = n
	}
	return w
}

// memberChanged is why repository repo is not what meta recorded, "" when it is, and
// how many files it had to read to tell.
func (e *Engine) memberChanged(repo string, meta facts.SnapshotMeta) (string, int) {
	name := filepath.Base(repo)
	if len(meta.FileHashes) == 0 {
		return name + ": no file hashes recorded", 0
	}
	if meta.Git != nil && meta.Git.Commit != "" {
		commit, err := runGit(repo, "rev-parse", "HEAD")
		if err != nil || commit != meta.Git.Commit {
			return name + ": another commit", 0
		}
	}
	files, _, _, _, err := e.walkRepo(context.Background(), repo)
	if err != nil {
		return name + ": " + err.Error(), 0
	}
	byPath := make(map[string]facts.FileHash, len(meta.FileHashes))
	for _, fh := range meta.FileHashes {
		byPath[fh.Path] = fh
	}
	read, matched := 0, 0
	for _, rel := range files {
		fh, ok := byPath[rel]
		info, err := os.Stat(filepath.Join(repo, rel))
		// The walk lists what the hash step then cannot read (a link to a directory, a
		// file it may not open), and the hash step leaves those out: so does this.
		if err != nil || !info.Mode().IsRegular() {
			if ok {
				return name + ": " + rel + " unreadable", read
			}
			continue
		}
		if !ok {
			if f, err := os.Open(filepath.Join(repo, rel)); err != nil {
				continue
			} else {
				_ = f.Close()
			}
			return name + ": " + rel + " added", read
		}
		matched++
		// The recorded time was taken before the file was read, and left empty when the
		// file had just been modified (hashFilesWithStat), so the same size and time
		// mean the content the recorded hash describes.
		if fh.ModTime != "" && fh.Size == info.Size() && fh.ModTime == info.ModTime().UTC().Format(time.RFC3339) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(repo, rel))
		if err != nil {
			return name + ": " + rel + " unreadable", read
		}
		read++
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != fh.Hash {
			return name + ": " + rel + " changed", read
		}
	}
	if matched != len(meta.FileHashes) {
		return fmt.Sprintf("%s: %d files removed", name, len(meta.FileHashes)-matched), read
	}
	return "", read
}
