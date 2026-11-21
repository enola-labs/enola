package facts

// Reach is what a client route's PropClientReachable says about the call it records.
type Reach int

const (
	// ReachUnjudged: the extractor could not tell whether the application runs the
	// code that makes the call. A client in a language the reach pass does not read,
	// a library its consumers call, a test path. Not a verdict either way.
	ReachUnjudged Reach = iota
	// ReachRuns: the caller is reached from the application's entry points.
	ReachRuns
	// ReachNeverRuns: nothing that runs reaches the caller. The call is in the source
	// and is never sent.
	ReachNeverRuns
)

func (r Reach) String() string {
	switch r {
	case ReachRuns:
		return "runs"
	case ReachNeverRuns:
		return "never runs"
	}
	return "unjudged"
}

// ClientReach reads PropClientReachable as the three states its contract names.
// Every reader of the prop goes through it, so that the absent state cannot be read
// as "runs" on one surface and as "unknown" on another.
func ClientReach(f Fact) Reach {
	switch f.PropAny(PropClientReachable) {
	case true:
		return ReachRuns
	case false:
		return ReachNeverRuns
	}
	return ReachUnjudged
}
