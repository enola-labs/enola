package history

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"os"
)

// lineSource yields a revision's fact lines in ascending order. The slice handed to yield
// is valid only during that call.
//
// A source is read twice per revision, once to measure and once to write, so the lines
// of a large graph are never held: on the kernel, facts.jsonl is over a gigabyte, and
// holding it as strings while the store is still live is what used to set the peak.
type lineSource func(yield func(line []byte) error) error

// errUnsorted is returned by a file source whose lines are out of order. facts.jsonl is
// written sorted, so it means the file was written by something else, and the caller
// falls back to reading the lines in and sorting them.
var errUnsorted = errors.New("lines are not sorted")

// fileLines streams path's non-empty lines, failing with errUnsorted at the first line
// that sorts before the one ahead of it.
func fileLines(path string) lineSource {
	return func(yield func([]byte) error) error {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()

		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)
		var prev []byte
		have := false
		for sc.Scan() {
			b := sc.Bytes()
			if len(b) == 0 {
				continue
			}
			if have && bytes.Compare(prev, b) > 0 {
				return errUnsorted
			}
			prev, have = append(prev[:0], b...), true
			if err := yield(b); err != nil {
				return err
			}
		}
		return sc.Err()
	}
}

// sliceLines yields lines, which must already be sorted.
func sliceLines(lines []string) lineSource {
	return func(yield func([]byte) error) error {
		var buf []byte
		for _, l := range lines {
			buf = append(buf[:0], l...)
			if err := yield(buf); err != nil {
				return err
			}
		}
		return nil
	}
}

// mergeLines walks parent (sorted) against src as two sorted multisets, calling del for
// each line only parent has and add for each line only src has, in ascending order of
// line: exactly the order a revision stores its patch in, and exactly the lines
// pkghistory.ComputePatch would return. each, when set, sees every line of src.
func mergeLines(parent []string, src lineSource, del func(string) error, add func([]byte) error, each func([]byte)) error {
	j := 0
	err := src(func(b []byte) error {
		if each != nil {
			each(b)
		}
		for j < len(parent) && parent[j] < string(b) {
			if err := del(parent[j]); err != nil {
				return err
			}
			j++
		}
		if j < len(parent) && parent[j] == string(b) {
			j++
			return nil
		}
		return add(b)
	})
	if err != nil {
		return err
	}
	for ; j < len(parent); j++ {
		if err := del(parent[j]); err != nil {
			return err
		}
	}
	return nil
}

// lineHasher is pkghistory.HashLines over lines seen one at a time.
type lineHasher struct{ h hash.Hash }

func newLineHasher() *lineHasher { return &lineHasher{h: sha256.New()} }

func (l *lineHasher) add(line []byte) {
	l.h.Write(line)
	l.h.Write([]byte{'\n'})
}

func (l *lineHasher) sum() string { return "sha256:" + hex.EncodeToString(l.h.Sum(nil)) }

// factSource is where c's fact lines come from: facts.jsonl streamed from disk, or the
// lines handed over, sorted.
//
// Sorted on the way in. A reconstruction always comes back sorted (Patch.Apply says so),
// so hashing the caller's ORDER rather than the canonical one produces a blob that can
// never verify: it fails the integrity check on first read, reporting damage where there
// is none. facts.jsonl is already sorted, and fileLines refuses it if it is not.
func factSource(c Contents) lineSource {
	if c.FactLines == nil && c.FactsPath != "" {
		return fileLines(c.FactsPath)
	}
	return sliceLines(sortedLines(c.FactLines))
}

// measureLines returns the hash of src's lines and the byte size of the patch taking
// parent to them (pkghistory.Patch.Size), holding none of them.
func measureLines(parent []string, src lineSource) (string, int, error) {
	hasher := newLineHasher()
	size := 0
	err := mergeLines(parent, src,
		func(line string) error { size += len(line) + 2; return nil },
		func(line []byte) error { size += len(line) + 2; return nil },
		hasher.add)
	return hasher.sum(), size, err
}
