package history

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// A revision's stored form is a PATCH OVER CANONICAL LINES, and the reason is a property
// of how enola serializes facts rather than a compression trick.
//
// facts.Store.WriteJSONL marshals every fact and then sorts the resulting LINES. So a
// snapshot's facts are exactly a sorted multiset of canonical strings, and facts.jsonl is
// a pure function of that multiset — insertion order, extraction order and map iteration
// order cannot reach the output. Two snapshots therefore differ by exactly two set
// differences: the lines this one has and its parent did not, and the reverse.
//
// That makes reconstruction set arithmetic on strings, which is the whole point. There is
// no fact identity to get right, no positional pairing inside key groups that collide
// (facts DO collide under diff's factKey — that is why groupByKey exists), no special case
// for the line-only moves diff.Compute deliberately discards, and no marshal round-trip
// that could reintroduce a byte difference. A reconstruction cannot be subtly wrong. It is
// right, or it fails the hash check in Revision.Verify.
//
// A semantic delta would be about 1.85× smaller (2,338 bytes against 4,333, measured on two
// real consecutive snapshots of this repository). That is not worth buying back a class of
// defect whose failure mode is silently rendering a past that never existed.

// Patch is the difference between two sorted multisets of canonical lines.
type Patch struct {
	// Del are lines the parent had and this revision does not.
	Del []string
	// Add are lines this revision has and the parent did not.
	Add []string
}

// Empty reports whether the patch changes nothing.
func (p Patch) Empty() bool { return len(p.Add) == 0 && len(p.Del) == 0 }

// Size is the patch's uncompressed byte cost, used to decide when a delta has grown large
// enough that a fresh base is cheaper than continuing to chain.
func (p Patch) Size() int {
	n := 0
	for _, l := range p.Add {
		n += len(l) + 2
	}
	for _, l := range p.Del {
		n += len(l) + 2
	}
	return n
}

// ComputePatch returns the patch taking prev to cur. Both are canonical line multisets;
// neither is modified.
//
// Multisets, not sets: two facts can serialize to the identical line (Swift declares the
// same class under mutually exclusive #if branches; a file imports one target twice). The
// counts are part of the snapshot, so a duplicate that disappears is a real removal, and
// treating the input as a set would silently drop it and produce a reconstruction one line
// short of the truth.
func ComputePatch(prev, cur []string) Patch {
	prevCount := counts(prev)
	curCount := counts(cur)

	var p Patch
	for line, n := range curCount {
		if extra := n - prevCount[line]; extra > 0 {
			for i := 0; i < extra; i++ {
				p.Add = append(p.Add, line)
			}
		}
	}
	for line, n := range prevCount {
		if gone := n - curCount[line]; gone > 0 {
			for i := 0; i < gone; i++ {
				p.Del = append(p.Del, line)
			}
		}
	}
	sort.Strings(p.Add)
	sort.Strings(p.Del)
	return p
}

// Apply returns prev with the patch applied, as a sorted line multiset.
//
// A Del naming a line prev does not contain is an error rather than a no-op: it means the
// chain was applied to the wrong parent, and continuing would produce a plausible-looking
// snapshot that never existed. Failing here names the revision; failing at the hash check
// two members later does not.
func (p Patch) Apply(prev []string) ([]string, error) {
	remaining := counts(prev)
	for _, line := range p.Del {
		if remaining[line] == 0 {
			return nil, fmt.Errorf("patch removes a line the parent does not have (%s) — applied to the wrong parent",
				truncate(line, 80))
		}
		remaining[line]--
	}

	out := make([]string, 0, len(prev)-len(p.Del)+len(p.Add))
	for line, n := range remaining {
		for i := 0; i < n; i++ {
			out = append(out, line)
		}
	}
	out = append(out, p.Add...)
	sort.Strings(out)
	return out, nil
}

func counts(lines []string) map[string]int {
	m := make(map[string]int, len(lines))
	for _, l := range lines {
		m[l]++
	}
	return m
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// Revision is one stored revision: its provenance, and the patches taking its parent to it.
//
// A BASE is a revision with no parent, whose patches are pure additions. That is not a
// special case bolted on — it is what "a patch against the empty set" already means — so
// there is one encoder, one decoder and one apply path rather than two that could disagree
// about a corner.
type Revision struct {
	// Parent is the facts hash of the revision this one is a patch against, empty for a base.
	Parent string
	// FactsHash is "sha256:…" over the reconstructed facts.jsonl bytes — the check that
	// makes reconstruction self-verifying.
	FactsHash string
	// Receipt is this revision's provenance, carried per revision (not per segment) because
	// diff.compareMeta needs both sides' version, config hash, glob hash and plugin sets to
	// judge two revisions comparable — and Entry.Epoch is a one-way hash of exactly those,
	// enough to know two revisions differ and never enough to say how.
	Receipt facts.Receipt
	// Facts and Insights are patches over canonical lines.
	Facts    Patch
	Insights Patch
}

const revisionMagic = "enola-history-revision 1"

// Encode writes the revision in the on-disk text format. The caller gzips it.
//
// Line-oriented rather than a JSON envelope for two reasons: JSON-encoding a fact line
// escapes every quote inside it, inflating the payload before compression ever sees it;
// and a patch that stays greppable is what `blame` scans in P3.
//
// Add and Del lines are emitted SORTED TOGETHER rather than in separate blocks, so the two
// halves of a line-only move (a fact whose body is identical and whose line number moved)
// land adjacent and gzip sees the redundancy. Measured on two real snapshots: 4,333 bytes
// interleaved against 5,451 split — a 20% saving for an ordering choice. On this repository
// 65 of the 82 changed lines are such moves, so it is the common case rather than a corner.
func (r Revision) Encode(w io.Writer) error {
	receiptJSON, err := json.Marshal(r.Receipt)
	if err != nil {
		return fmt.Errorf("encoding receipt: %w", err)
	}

	var b bytes.Buffer
	b.WriteString(revisionMagic + "\n")
	if r.Parent != "" {
		fmt.Fprintf(&b, "parent %s\n", r.Parent)
	}
	fmt.Fprintf(&b, "facts %s\n", r.FactsHash)
	fmt.Fprintf(&b, "receipt %d\n", len(receiptJSON))
	b.Write(receiptJSON)
	b.WriteByte('\n')

	writeSection := func(name string, p Patch) {
		b.WriteString(name + "\n")
		for _, line := range interleave(p) {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	writeSection("facts", r.Facts)
	writeSection("insights", r.Insights)

	_, err = w.Write(b.Bytes())
	return err
}

// interleave renders a patch as "+line" / "-line" entries ordered by the line itself, so
// near-identical add/remove pairs sit next to each other. The sign breaks ties, and it is
// '-' before '+' so a removal reads before the replacement that follows it.
func interleave(p Patch) []string {
	out := make([]string, 0, len(p.Add)+len(p.Del))
	for _, l := range p.Del {
		out = append(out, "-"+l)
	}
	for _, l := range p.Add {
		out = append(out, "+"+l)
	}
	sort.Slice(out, func(i, j int) bool {
		li, lj := out[i][1:], out[j][1:]
		if li != lj {
			return li < lj
		}
		// Unreachable in practice — ComputePatch emits a given line as an addition or a
		// removal, never both — but ordering must be total for the output to be
		// byte-reproducible.
		return out[i][0] < out[j][0]
	})
	return out
}

// DecodeRevision parses the format Encode writes.
func DecodeRevision(r io.Reader) (*Revision, error) {
	br := bufio.NewReader(r)
	magic, err := br.ReadString('\n')
	if err != nil || strings.TrimRight(magic, "\n") != revisionMagic {
		return nil, fmt.Errorf("not an enola history revision (got %q)", truncate(strings.TrimRight(magic, "\n"), 40))
	}

	rev := &Revision{}
	// Header: key/value lines until "receipt <n>", which is followed by exactly n bytes and
	// ends the header. An unknown field is an ERROR rather than something to skip past: this
	// is an integrity format, and quietly ignoring a field a newer writer considered load
	// bearing is how a reader renders a past that never happened. The version in the magic
	// line is the escape hatch for changing it.
	for done := false; !done; {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("truncated header: %w", err)
		}
		key, value, _ := strings.Cut(strings.TrimRight(line, "\n"), " ")
		switch key {
		case "parent":
			rev.Parent = value
		case "facts":
			rev.FactsHash = value
		case "receipt":
			n, err := strconv.Atoi(value)
			if err != nil {
				return nil, fmt.Errorf("bad receipt length %q: %w", value, err)
			}
			buf := make([]byte, n)
			if _, err := io.ReadFull(br, buf); err != nil {
				return nil, fmt.Errorf("truncated receipt: %w", err)
			}
			if err := json.Unmarshal(buf, &rev.Receipt); err != nil {
				return nil, fmt.Errorf("parsing receipt: %w", err)
			}
			if _, err := br.ReadString('\n'); err != nil {
				return nil, fmt.Errorf("truncated after receipt: %w", err)
			}
			done = true
		default:
			return nil, fmt.Errorf("unknown header field %q", key)
		}
	}

	target := (*Patch)(nil)
	sc := bufio.NewScanner(br)
	sc.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		switch line {
		case "facts":
			target = &rev.Facts
			continue
		case "insights":
			target = &rev.Insights
			continue
		case "":
			continue
		}
		if target == nil {
			return nil, fmt.Errorf("patch line before any section header")
		}
		switch line[0] {
		case '+':
			target.Add = append(target.Add, line[1:])
		case '-':
			target.Del = append(target.Del, line[1:])
		default:
			return nil, fmt.Errorf("patch line starts with %q, want '+' or '-'", line[0])
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading patch: %w", err)
	}
	sort.Strings(rev.Facts.Add)
	sort.Strings(rev.Facts.Del)
	sort.Strings(rev.Insights.Add)
	sort.Strings(rev.Insights.Del)
	return rev, nil
}

// HashLines returns the "sha256:"-prefixed digest of the facts.jsonl bytes these lines
// serialize to — the same bytes facts.Store.WriteJSONL writes, so the result is directly
// comparable with the receipt's recorded output hash for facts.jsonl.
func HashLines(lines []string) string {
	h := sha256.New()
	for _, l := range lines {
		h.Write([]byte(l))
		h.Write([]byte{'\n'})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}
