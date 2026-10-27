package facts

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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
