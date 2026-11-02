package facts

import (
	"crypto/sha256"
	"encoding/hex"
)

// Fact identity for EXTERNAL consumers.
//
// A relation names its target by NAME, and names repeat: a snapshot of any size
// carries several facts under one name, and a consumer that re-materializes the
// graph in its own database must guess which one an edge meant. Cognee guesses
// with an exact match, an unqualified-suffix fallback and a same-repo
// preference, and DROPS the edge when the guess is ambiguous. The ids below
// replace the guess.
//
// They exist only in facts.jsonl. Nothing inside enola reads them: the graph is
// name-keyed (see NewGraph), diff keys on its own factKey, and the history store
// keeps serialized lines. That is deliberate — an id that no internal reader
// depends on cannot change an internal answer, so adding one cannot regress the
// analysis. It is computed at serialization and never stored on a Fact, which
// also keeps it off a struct that exists 39M times on the largest graphs.

// idBytes is the identity's width in bytes. 128 bits is far past what the
// birthday bound needs: at 39M facts — the largest graph measured — the odds of
// any collision are about 1 in 10^14, and a collision is the one failure this
// exists to prevent (two distinct facts merging into one node is exactly the
// bug being fixed, arriving by a different door).
const idBytes = 16

// FactID returns the stable identity of a fact as 32 lowercase hex characters.
//
// The identity is (repo, kind, name, file). Name alone is not enough — two
// functions sharing a name in different files are distinct facts, and merging
// them is the loss this fixes — and file alone is not enough either, since one
// file declares many facts.
//
// It is a pure function of those four strings: the same tree yields the same id
// on any machine and in any run, matching the byte-stability guarantee the rest
// of the artifact makes. It does NOT include line or column, so an id survives
// code moving down a file; that is the point of an identity, and it is why an id
// stays comparable across two snapshots of a repository that edited something
// above it.
//
// IDENTITY, NOT UNIQUENESS. Facts can share all four fields — one file importing
// the same target at two statements, two overloads declared at different lines —
// and such facts share an id. Measured on a 20,808-fact snapshot: 482 ids cover
// 706 facts. Those repeats are the same thing recorded twice, so a consumer
// keying nodes on the id merges them, which is the correct outcome. Adding line
// would not fix it (244 repeats survive it) and would cost the stability above.
func FactID(repo, kind, name, file string) string {
	id, _ := factIDInto(nil, repo, kind, name, file)
	return id
}

// factIDInto is FactID over a caller-owned scratch buffer, returning the buffer
// so the next call can reuse it.
//
// The serialization path computes one id per fact plus one per resolved relation
// — several million on a large graph — and allocation COUNT is what the memory
// ratchet grades. Written the obvious way (sha256.New, Sum(nil), EncodeToString)
// each id costs three allocations; this costs one, the returned string, which is
// the only part that has to outlive the call.
func factIDInto(scratch []byte, repo, kind, name, file string) (string, []byte) {
	// NUL between the fields, so ("a", "b") cannot hash as ("ab", ""). No field
	// may contain a NUL: they are a repo label, a registered kind, a symbol name
	// and a path.
	scratch = scratch[:0]
	scratch = append(scratch, repo...)
	scratch = append(scratch, 0)
	scratch = append(scratch, kind...)
	scratch = append(scratch, 0)
	scratch = append(scratch, name...)
	scratch = append(scratch, 0)
	scratch = append(scratch, file...)
	// Sum256 returns an array by value; sha256.New would put the digest state on
	// the heap instead.
	sum := sha256.Sum256(scratch)
	return hex.EncodeToString(sum[:idBytes]), scratch
}

// identityFile is the path an id hashes: relative to the repository, whatever a multi-repo
// snapshot prefixed for display, so a declaration has one id alone and at any place in a
// cluster. It takes the flag rather than looking for the prefix: repository beta can hold a
// directory beta, and beta/code.py alone is not the prefixed form of code.py.
func identityFile(repo, file string, prefixed bool) string {
	if prefixed && len(file) > len(repo) && file[len(repo)] == '/' && file[:len(repo)] == repo {
		return file[len(repo)+1:]
	}
	return file
}

// IdentityFile returns the path this fact's id hashes.
func (f Fact) IdentityFile() string { return identityFile(f.Repo, f.File, f.RepoPrefixed) }

// Identity returns this fact's FactID.
func (f Fact) Identity() string { return FactID(f.Repo, f.Kind, f.Name, f.IdentityFile()) }

// sameIdentity reports whether two facts carry the same id without computing
// either: equal inputs to FactID mean an equal id, and comparing four strings is
// cheaper than two hashes.
func sameIdentity(a, b Fact) bool {
	return a.Repo == b.Repo && a.Kind == b.Kind && a.Name == b.Name && a.File == b.File
}
