// Package hookstate records when enola's agent hooks last ran.
//
// It exists because of a defect that no test in this repository could have caught.
// `enola install --hooks` wrote the Stop hook in a JSON shape the agent ignored, so
// the hooks never fired — and every cheaper check passed while it was broken: the
// installer wrote the file it meant to, the hook binary produced the right verdict
// when invoked by hand, and a unit test asserted the shape against the same belief
// that produced it.
//
// The shape of that config is a contract owned by the agent, which ships on its own
// schedule and can change after enola is released. So it is not testable here in any
// durable way: the only defence that survives the contract moving is noticing, on the
// machine where it matters, that the hooks have stopped firing.
//
// Hence a heartbeat. Every hook invocation records that it ran — INCLUDING the runs
// where it deliberately says nothing, which is the overwhelming majority and the whole
// point. Silence was the defect's entire signature; a hook that has never fired is now
// distinguishable from one that fires and finds nothing to report.
package hookstate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// FileName is the heartbeat file, written inside the engine's output directory.
//
// It is deliberately NOT one of engine.snapshotArtifactFiles, so `baseline pin` does
// not copy it into baseline/ and a heartbeat can never be mistaken for snapshot state.
// The output directory is excluded from indexing, so this file contributes no facts
// and cannot affect a snapshot ID.
const FileName = "hooks.json"

// Outcome is what a hook run concluded. Recorded because "fired 40 times and always
// declined to grade" and "fired 40 times and found nothing wrong" look identical from
// a timestamp alone, and only one of them is a problem.
type Outcome string

const (
	// OutcomeReported: a regression was found and handed back to the agent.
	OutcomeReported Outcome = "reported"
	// OutcomeClean: graded successfully, nothing to report. The normal case.
	OutcomeClean Outcome = "clean"
	// OutcomeDeclined: the baseline was not comparable, so no verdict was possible.
	// The hook stays silent in-session; this is where that silence becomes visible.
	OutcomeDeclined Outcome = "declined"
	// OutcomeUnavailable: nothing to grade against — usually no baseline pinned yet.
	OutcomeUnavailable Outcome = "unavailable"
	// OutcomePinned: the session-start hook froze a baseline.
	OutcomePinned Outcome = "pinned"
	// OutcomeSkipped: the session-start hook deliberately did no work — a deliberate
	// baseline it must not replace, an unchanged tree, or another session holding the lock.
	OutcomeSkipped Outcome = "skipped"
)

// Event names a hook. These match the `enola hook <event>` subcommands.
type Event string

const (
	EventStop         Event = "stop"
	EventSessionStart Event = "session-start"
)

// Record is one event's history. Count is cumulative; the timestamps bound it.
type Record struct {
	FirstFired  time.Time `json:"first_fired"`
	LastFired   time.Time `json:"last_fired"`
	Count       int       `json:"count"`
	LastOutcome Outcome   `json:"last_outcome,omitempty"`

	// LastReason identifies WHAT was last reported, so a repeat can be recognised
	// and left unsaid. Empty whenever the last run reported nothing, including a run
	// that graded clean, which is what makes a recurrence speak again instead of
	// being suppressed forever by a problem that was fixed in between.
	//
	// It began as the decline identity alone, and covering only that path is what
	// let the regression and unenforced reports repeat at every Stop. It now holds
	// check.Verdict.ReportKey() for all three.
	//
	// Absent from older heartbeat files, where it reads as the zero value: an
	// upgrade therefore reports the current state once, which is the right
	// behaviour rather than a migration.
	LastReason string `json:"last_reason,omitempty"`

	// LastSession is the agent session LastReason was decided in, so a suppression
	// cannot outlive the session that earned it.
	//
	// Without it the rule is keyed on nothing but the repository, and a DELIBERATELY
	// pinned baseline is never replaced by the session-start hook: the same verdict
	// then stands for days, so the report would be made once ever and every later
	// session would be told nothing at all. A regression going permanently quiet is
	// worse than repeating it.
	//
	// Empty when the payload carried no session id, which reduces to the older
	// repository-wide behaviour rather than to reporting on every Stop.
	LastSession string `json:"last_session,omitempty"`
}

// State is the whole file.
type State struct {
	// InstalledAt is when `install --hooks` last configured this repository. It is what
	// makes "never fired" meaningful: without it, an empty file cannot be told apart
	// from hooks that were never installed in the first place.
	InstalledAt time.Time         `json:"installed_at,omitempty"`
	Events      map[Event]*Record `json:"events,omitempty"`
	// HookCommand records which binary the installed hooks invoke, so a heartbeat that
	// stopped can be attributed to a moved or replaced enola rather than to the agent.
	HookCommand string `json:"hook_command,omitempty"`
}

// Fired reports whether the event has ever run.
func (s State) Fired(e Event) bool {
	r, ok := s.Events[e]
	return ok && r.Count > 0
}

// Get returns the record for an event, or nil.
func (s State) Get(e Event) *Record { return s.Events[e] }

// Path returns the heartbeat file's location for an output directory.
func Path(outDir string) string { return filepath.Join(outDir, FileName) }

// Load reads the heartbeat. A missing or unreadable file is not an error: it means
// "nothing recorded", which is a legitimate state and the one a fresh repository is in.
func Load(outDir string) State {
	var s State
	data, err := os.ReadFile(Path(outDir))
	if err != nil {
		return s
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return State{} // a corrupt heartbeat is worth less than no heartbeat
	}
	return s
}

// Report identifies WHAT a hook said and WHICH session it said it in. Both halves are
// needed: the key tells a repeat from something new without diffing prose, and the
// session keeps a suppression from outliving the session that earned it.
type Report struct {
	// Key comes from check.Verdict.ReportKey(). Empty for every run that said
	// nothing, which is what clears a previously reported key so the same problem
	// returning after being fixed is reported again rather than suppressed forever.
	Key string
	// Session is the agent's session id, or "" when the payload carried none.
	Session string
}

// RecordFired stamps an event. Best-effort and silent by contract: this is called from
// a hook, and a hook that cannot fail loudly must not try. Every error path returns
// without disturbing the caller.
func RecordFired(outDir string, e Event, o Outcome) {
	RecordFiredWithReport(outDir, e, o, Report{})
}

// RecordFiredWithReport is RecordFired carrying the identity of what the run reported.
// Pass the zero Report for every run that reported nothing: that is what clears a
// previously recorded key.
func RecordFiredWithReport(outDir string, e Event, o Outcome, rep Report) {
	mutate(outDir, func(s *State) {
		r := recordFor(s, e)
		r.LastFired = now()
		r.Count++
		r.LastOutcome = o
		r.LastReason = rep.Key
		r.LastSession = rep.Session
	})
}

// RecordSuppressed stamps a run that fired and deliberately said nothing because the
// harness was already replaying a report this hook had already made.
//
// It touches ONLY the timestamp and the count. Writing an outcome here would overwrite
// the verdict of the run that actually graded something, and writing a reason (even an
// empty one) would clear the key the suppression depends on, re-arming the identical
// report and reinstating the very loop through the dedupe that the flag just broke.
//
// Recorded rather than skipped because the heartbeat is the only place a change in that
// flag's meaning would ever surface: "fires and always suppresses" and "fires and
// reports" are different states, and one of them is the hook silently not running.
func RecordSuppressed(outDir string, e Event) {
	mutate(outDir, func(s *State) {
		r := recordFor(s, e)
		r.LastFired = now()
		r.Count++
	})
}

// recordFor returns the event's record, creating it on first use.
func recordFor(s *State, e Event) *Record {
	if s.Events == nil {
		s.Events = map[Event]*Record{}
	}
	r := s.Events[e]
	if r == nil {
		r = &Record{FirstFired: now()}
		s.Events[e] = r
	}
	return r
}

// ShouldReport reports whether what a hook is about to say is worth saying out loud.
//
// True when it differs from what was last recorded: a first occurrence, a different
// problem, the same problem returning after a clean grade cleared it, or the same
// problem in a NEW session. False for an unchanged repeat inside one session.
//
// That last case is not a matter of taste. A Stop hook's output does not annotate a
// finished turn, it prevents the turn from ending: the harness feeds the text back to
// the model and stops again afterwards. So an identical repeat costs the user a whole
// model turn, and repeating it costs one per Stop until the harness overrides the hook
// at its consecutive-block cap and the session ends on a warning instead of on the
// report. The standing state is visible in `enola doctor` either way.
func ShouldReport(outDir string, e Event, rep Report) bool {
	if rep.Key == "" {
		return false
	}
	r := Load(outDir).Get(e)
	return r == nil || r.LastReason != rep.Key || r.LastSession != rep.Session
}

// RecordInstalled stamps the install time and the hook command, so a later report can
// say "installed on X, never fired since".
func RecordInstalled(outDir, hookCommand string) {
	mutate(outDir, func(s *State) {
		s.InstalledAt = now()
		s.HookCommand = hookCommand
	})
}

// Clear removes the heartbeat, for `uninstall`. A stale "never fired since <date>"
// after the hooks were deliberately removed would be a false alarm.
func Clear(outDir string) { _ = os.Remove(Path(outDir)) }

// now is a variable so tests can pin it.
var now = func() time.Time { return time.Now().UTC().Truncate(time.Second) }

// mutate applies fn to the current state and writes it back atomically.
//
// Read-modify-write without locking is deliberate. Several agent sessions on one
// repository is the documented normal case, so a concurrent update can lose a count —
// which costs nothing, because the question this answers is "has it fired at all, and
// when last", not "exactly how many times". What must NOT happen is a torn file, and
// the temp-plus-rename below makes that impossible: a reader sees either the old file
// or the new one. Taking a lock here would mean a hook waiting on another hook, which
// is exactly what these hooks are built never to do.
func mutate(outDir string, fn func(*State)) {
	if outDir == "" {
		return
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return
	}
	s := Load(outDir)
	fn(&s)

	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	data = append(data, '\n')

	// Staged in the same directory so the rename stays on one filesystem: across a
	// mount boundary os.Rename fails with EXDEV and the atomicity is lost.
	tmp, err := os.CreateTemp(outDir, "."+FileName+".tmp-")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return
	}
	if err := os.Rename(tmpName, Path(outDir)); err != nil {
		_ = os.Remove(tmpName)
	}
}
