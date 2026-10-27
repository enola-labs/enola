package facts

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// PropBodyHash is the version of a symbol: a hash of the source its span covers.
//
// The id names a declaration and stays the same while its body is rewritten, and the
// line moves whenever anything above it changes. Neither says whether the code itself
// changed, which is what anything remembering a fact about a function needs to know:
// a note about a function is stale when its body changes, not when a commit lands.
//
// Like PropDoc it says nothing about the architecture, so the diff leaves it out of
// what it compares. It is set only on symbols whose extractor measured a span.
const PropBodyHash = "body_hash"

// BodyHash hashes the lines start..end (1-based, inclusive) of src.
//
// Each line is trimmed and blank lines are dropped, so re-indenting a function (wrapping
// it in a block, a formatter changing tabs) or moving it does not change its version,
// and editing a single token does. It returns "" when the span is not inside src.
func BodyHash(src []byte, start, end int) string {
	if start <= 0 || end < start {
		return ""
	}
	h := sha256.New()
	line, wrote := 1, false
	for len(src) > 0 && line <= end {
		next := bytes.IndexByte(src, '\n')
		cur := src
		if next >= 0 {
			cur, src = src[:next], src[next+1:]
		} else {
			src = nil
		}
		if line >= start {
			if t := bytes.TrimSpace(cur); len(t) > 0 {
				if wrote {
					h.Write([]byte{'\n'})
				}
				h.Write(t)
				wrote = true
			}
		}
		line++
	}
	if line <= start {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// NormalizedLines returns lines start..end (1-based, inclusive) of src the way BodyHash
// reads them: trimmed, blank lines dropped. ok is false when the span is not inside src.
//
// It is BodyHash's reading, not a second one: HashLines(NormalizedLines(src, a, b)) is
// BodyHash(src, a, b). BodyHash streams instead of building the slice because it runs
// for every symbol of every snapshot; this is for the few callers that need the lines
// themselves, to look for a run of them inside a body.
func NormalizedLines(src []byte, start, end int) (lines []string, ok bool) {
	if start <= 0 || end < start {
		return nil, false
	}
	line := 1
	for len(src) > 0 && line <= end {
		next := bytes.IndexByte(src, '\n')
		cur := src
		if next >= 0 {
			cur, src = src[:next], src[next+1:]
		} else {
			src = nil
		}
		if line >= start {
			if t := bytes.TrimSpace(cur); len(t) > 0 {
				lines = append(lines, string(t))
			}
		}
		line++
	}
	return lines, line > start
}

// HashLines hashes normalized lines the way BodyHash hashes a span.
func HashLines(lines []string) string {
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:8])
}

// DeclaredName is a symbol's name without its module: `ElementStore.add` for
// `packages/element/src.ElementStore.add`. A module is a path, and the symbol starts
// after the first dot past its last slash.
//
// It is what a symbol keeps when it moves to another file or package, and with its
// body hash what pairs the old id with the new one. The whole declared name, not the
// last segment, because one-line members are where bodies collide most: two props
// interfaces each declaring `title?: string;` are two members named `title`, and only
// their parents tell them apart.
func DeclaredName(name string) string {
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		name = name[i+1:]
	}
	if i := strings.IndexByte(name, '.'); i >= 0 {
		return name[i+1:]
	}
	return name
}
