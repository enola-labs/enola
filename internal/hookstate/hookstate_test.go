package hookstate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestLoad_MissingFileIsNotAnError(t *testing.T) {
	s := Load(t.TempDir())
	if s.Fired(EventStop) || s.Fired(EventSessionStart) {
		t.Error("a repository with no heartbeat must report nothing fired")
	}
	if !s.InstalledAt.IsZero() {
		t.Error("InstalledAt should be zero when nothing was recorded")
	}
}

// The distinction the whole package exists for: a hook that fired and found nothing is
// not a hook that never fired, even though both are silent in the session.
func TestRecordFired_SilentRunIsStillRecorded(t *testing.T) {
	dir := t.TempDir()
	RecordFired(dir, EventStop, OutcomeClean)

	s := Load(dir)
	if !s.Fired(EventStop) {
		t.Fatal("a clean (silent) stop run must still be recorded as fired")
	}
	r := s.Get(EventStop)
	if r.Count != 1 {
		t.Errorf("Count = %d, want 1", r.Count)
	}
	if r.LastOutcome != OutcomeClean {
		t.Errorf("LastOutcome = %q, want %q", r.LastOutcome, OutcomeClean)
	}
	if r.FirstFired.IsZero() || r.LastFired.IsZero() {
		t.Error("both timestamps should be set on the first run")
	}
}

func TestRecordFired_AccumulatesAndKeepsFirstFired(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 7, 31, 9, 0, 0, 0, time.UTC)
	restore := now
	defer func() { now = restore }()

	now = func() time.Time { return base }
	RecordFired(dir, EventStop, OutcomeClean)
	now = func() time.Time { return base.Add(2 * time.Hour) }
	RecordFired(dir, EventStop, OutcomeReported)

	r := Load(dir).Get(EventStop)
	if r.Count != 2 {
		t.Errorf("Count = %d, want 2", r.Count)
	}
	if !r.FirstFired.Equal(base) {
		t.Errorf("FirstFired moved: %v, want %v", r.FirstFired, base)
	}
	if !r.LastFired.Equal(base.Add(2 * time.Hour)) {
		t.Errorf("LastFired = %v, want the later stamp", r.LastFired)
	}
	if r.LastOutcome != OutcomeReported {
		t.Errorf("LastOutcome = %q, want the most recent one", r.LastOutcome)
	}
}

func TestRecordFired_EventsAreIndependent(t *testing.T) {
	dir := t.TempDir()
	RecordFired(dir, EventSessionStart, OutcomePinned)

	s := Load(dir)
	if !s.Fired(EventSessionStart) {
		t.Error("session-start should be recorded")
	}
	// The half-installed case this package is meant to surface.
	if s.Fired(EventStop) {
		t.Error("stop must NOT be reported as fired just because session-start did")
	}
}

func TestRecordInstalled_MakesNeverFiredMeaningful(t *testing.T) {
	dir := t.TempDir()
	RecordInstalled(dir, "/usr/local/bin/enola")

	s := Load(dir)
	if s.InstalledAt.IsZero() {
		t.Error("InstalledAt not recorded")
	}
	if s.HookCommand != "/usr/local/bin/enola" {
		t.Errorf("HookCommand = %q", s.HookCommand)
	}
	if s.Fired(EventStop) {
		t.Error("installing must not look like firing")
	}
}

func TestRecordInstalled_PreservesExistingHistory(t *testing.T) {
	dir := t.TempDir()
	RecordFired(dir, EventStop, OutcomeClean)
	RecordInstalled(dir, "enola")

	if !Load(dir).Fired(EventStop) {
		t.Error("re-installing must not erase the run history")
	}
}

func TestClear_RemovesTheRecord(t *testing.T) {
	dir := t.TempDir()
	RecordFired(dir, EventStop, OutcomeClean)
	Clear(dir)

	if Load(dir).Fired(EventStop) {
		t.Error("Clear should remove the heartbeat, so uninstall leaves no false alarm")
	}
	if _, err := os.Stat(Path(dir)); !os.IsNotExist(err) {
		t.Error("the file itself should be gone")
	}
}

// A hook must never fail loudly, so every error path has to be silent and harmless.
func TestRecordFired_UnwritableDirIsSilent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nope")
	if err := os.WriteFile(dir, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	RecordFired(dir, EventStop, OutcomeClean) // must not panic
	RecordFired("", EventStop, OutcomeClean)  // nor on an empty path
}

func TestLoad_CorruptFileDegradesToEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(Path(dir), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if Load(dir).Fired(EventStop) {
		t.Error("a corrupt heartbeat must read as 'nothing recorded', not crash or lie")
	}
}

// Concurrent sessions on one repository are the documented normal case. A lost count is
// acceptable; a torn file that reads back as corrupt is not.
func TestRecordFired_ConcurrentWritesLeaveValidJSON(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			RecordFired(dir, EventStop, OutcomeClean)
		}()
	}
	wg.Wait()

	data, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatalf("heartbeat missing after concurrent writes: %v", err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatalf("heartbeat is not valid JSON after concurrent writes: %v\n%s", err, data)
	}
	if !s.Fired(EventStop) {
		t.Error("at least one of the concurrent writes should have landed")
	}
}

// The heartbeat lives in the output directory but must never be mistaken for snapshot
// state: engine.copyArtifacts copies a fixed list, and this file is not on it.
func TestFileName_IsNotASnapshotArtifact(t *testing.T) {
	for _, artifact := range []string{"facts.jsonl", "insights.json", "snapshot.meta.json", "receipt.json"} {
		if FileName == artifact {
			t.Fatalf("%s collides with a snapshot artifact name", FileName)
		}
	}
}

// A suppressed run is the second Stop of a turn, when the harness is already replaying
// the report the first one made. It must count as a run and change nothing else.
//
// The reason it must not clear LastReason is the bug this was written for: clearing it
// re-arms the identical report, so the next Stop says it again and the loop comes back
// through the dedupe after the flag closed it off.
func TestRecordSuppressed_CountsTheRunAndKeepsTheReportIdentity(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 9, 12, 10, 0, 0, 0, time.UTC)
	restore := now
	defer func() { now = restore }()

	now = func() time.Time { return base }
	rep := Report{Key: "layers:Layer violation", Session: "s1"}
	RecordFiredWithReport(dir, EventStop, OutcomeReported, rep)

	now = func() time.Time { return base.Add(time.Minute) }
	RecordSuppressed(dir, EventStop)

	r := Load(dir).Get(EventStop)
	if r.Count != 2 {
		t.Errorf("Count = %d, want 2: a run that fired and said nothing still ran", r.Count)
	}
	if !r.LastFired.Equal(base.Add(time.Minute)) {
		t.Errorf("LastFired = %v, want the suppressed run's time", r.LastFired)
	}
	if r.LastOutcome != OutcomeReported {
		t.Errorf("LastOutcome = %q, want %q: a suppressed run must not overwrite the verdict of the run that graded",
			r.LastOutcome, OutcomeReported)
	}
	if r.LastReason != rep.Key || r.LastSession != rep.Session {
		t.Fatalf("a suppressed run cleared the report identity (%q/%q): that re-arms the identical report and the loop returns",
			r.LastReason, r.LastSession)
	}
	if ShouldReport(dir, EventStop, rep) {
		t.Error("the report must still be suppressed after a suppressed run")
	}
}

// The first suppressed run on a repository with no heartbeat yet must create one rather
// than panicking on a nil map.
func TestRecordSuppressed_OnAFreshRepository(t *testing.T) {
	dir := t.TempDir()
	RecordSuppressed(dir, EventStop)

	r := Load(dir).Get(EventStop)
	if r == nil || r.Count != 1 {
		t.Fatalf("a suppressed run on a fresh repository must record one run, got %+v", r)
	}
	if r.LastOutcome != "" {
		t.Errorf("LastOutcome = %q, want empty: nothing was graded", r.LastOutcome)
	}
	RecordSuppressed("", EventStop) // must stay silent, as every hook path does
}
