// Package diff re-exports enola's internal snapshot-diff engine as public type
// aliases and functions so code outside this module can compute and consume
// architecture deltas without importing the internal package directly.
//
// These are Go ALIASES, identical to the internal types.
package diff

import internal "github.com/enola-labs/enola/internal/diff"

// Delta types (aliases — identical to the internal types).
type (
	SnapshotDiff      = internal.SnapshotDiff
	Edge              = internal.Edge
	FactChange        = internal.FactChange
	Comparability     = internal.Comparability
	ReceiptComparison = internal.ReceiptComparison
	MetricDelta       = internal.MetricDelta
)

// Compute returns the delta from baseline to current. See internal/diff for the
// ratchet semantics (delta-only; pre-existing state never reported).
var Compute = internal.Compute

// KindCounts tallies facts by kind (used for structural-change summaries).
var KindCounts = internal.KindCounts

// CompareMeta reports whether two snapshots were generated over equivalent inputs.
var CompareMeta = internal.CompareMeta

// CompareReceipts compares two snapshot receipts (comparability + metric deltas
// + extraction-quality regressions).
var CompareReceipts = internal.CompareReceipts
