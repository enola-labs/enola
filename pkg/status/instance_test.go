package status

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestInstanceRoundTrip covers the basic registry contract: a written record is
// readable, and Close removes it.
func TestInstanceRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	start := time.Now()
	tr := NewTracker("/tmp/example-repo")
	tr.SetStartTime(start)
	tr.SetDashboardPort(45678)
	tr.SetIdentity(Identity{Binary: "enola", Version: "1.2.3", ConfigPath: "mcp-arch.yaml", WorkDir: "/tmp/wd"})
	tr.SetGraphFunc(func() GraphState {
		return GraphState{
			PrimaryRepo: "/tmp/example-repo",
			Repos:       []InstanceRepo{{Label: "api", Path: "/tmp/example-repo", FactCount: 42}},
			FactCount:   42,
		}
	})
	tr.PersistStartup()
	defer tr.Close()

	live := LiveInstances()
	if len(live) != 1 {
		t.Fatalf("LiveInstances() = %d records, want 1", len(live))
	}
	got := live[0]
	if got.PID != os.Getpid() {
		t.Errorf("PID = %d, want %d", got.PID, os.Getpid())
	}
	if got.Binary != "enola" || got.Version != "1.2.3" {
		t.Errorf("binary/version = %q/%q, want enola/1.2.3", got.Binary, got.Version)
	}
	if got.DashboardPort != 45678 || got.URL() != "http://127.0.0.1:45678" {
		t.Errorf("dashboard = %d / %q", got.DashboardPort, got.URL())
	}
	if got.RepoLabels() != "api" {
		t.Errorf("RepoLabels() = %q, want %q", got.RepoLabels(), "api")
	}

	tr.Close()
	if live := LiveInstances(); len(live) != 0 {
		t.Errorf("after Close: %d records remain, want 0", len(live))
	}
}

// TestLiveInstancesReapsDeadAndKeepsLive is the guarantee the dashboard's
// instance list rests on: records left behind by servers that died (a hard-killed
// agent terminal) must disappear, while the live ones stay. PID 0 is never a
// running user process, so it stands in for a dead server.
func TestLiveInstancesReapsDeadAndKeepsLive(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	now := time.Now()
	dead := Instance{PID: 0, StartTime: now.Add(-time.Hour), Heartbeat: now.Add(-time.Hour), Binary: "enola"}
	// A live PID whose record has not been refreshed for far longer than the
	// heartbeat interval: treated as stale, guarding against PID reuse.
	stalePID := Instance{PID: os.Getppid(), StartTime: now.Add(-24 * time.Hour), Heartbeat: now.Add(-24 * time.Hour)}
	alive := Instance{PID: os.Getpid(), StartTime: now, Heartbeat: now, Binary: "acme-arch"}

	for _, inst := range []Instance{dead, stalePID, alive} {
		if err := writeInstance(inst); err != nil {
			t.Fatalf("writeInstance(%d): %v", inst.PID, err)
		}
	}

	live := LiveInstances()
	if len(live) != 1 {
		t.Fatalf("LiveInstances() = %d, want 1 (only the running process)", len(live))
	}
	if live[0].PID != os.Getpid() {
		t.Errorf("survivor PID = %d, want %d", live[0].PID, os.Getpid())
	}

	// Reaping must also unlink the files, not merely filter them out.
	dir, err := instancesDir()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("registry holds %d files after reaping, want 1", len(entries))
	}
}

// TestInstanceFileNameSurvivesPIDReuse asserts the record key includes the start
// time, so a recycled PID cannot resurrect a dead instance's record.
func TestInstanceFileNameSurvivesPIDReuse(t *testing.T) {
	a := instanceFileName(4242, time.Unix(1, 0))
	b := instanceFileName(4242, time.Unix(2, 0))
	if a == b {
		t.Fatalf("same filename %q for two different start times", a)
	}
	if !strings.HasPrefix(a, "4242-") {
		t.Errorf("filename %q does not lead with the PID", a)
	}
}

// TestConcurrentTrackersDoNotLoseCounts is the regression test for the bug this
// registry work was built on: several servers share one per-repo usage file, and
// the old write path stamped baseline+ownSession over it. Two processes on one
// repo therefore overwrote each other and increments vanished.
//
// Two trackers here stand in for two servers on the same repo. Every recorded
// call must survive in the shared file.
func TestConcurrentTrackersDoNotLoseCounts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	const repo = "/tmp/shared-repo"
	const callsEach = 50

	a := NewTracker(repo)
	a.SetStartTime(time.Now())
	b := NewTracker(repo)
	b.SetStartTime(time.Now())

	var wg sync.WaitGroup
	for _, tr := range []*Tracker{a, b} {
		wg.Add(1)
		go func(tr *Tracker) {
			defer wg.Done()
			for range callsEach {
				tr.OnToolCall("explore", repo)
			}
		}(tr)
	}
	wg.Wait()

	path, err := usagePath(canonicalRepoPath(repo))
	if err != nil {
		t.Fatal(err)
	}
	info, _, err := ReadStatus(path)
	if err != nil {
		t.Fatalf("ReadStatus: %v", err)
	}
	if want := 2 * callsEach; info.ToolCounts["explore"] != want {
		t.Errorf("explore total = %d, want %d — concurrent servers lost counts",
			info.ToolCounts["explore"], want)
	}

	a.Close()
	b.Close()
}

// TestFlushMergesForeignIncrements checks the delta merge directly: counts a
// sibling process wrote between two of our flushes must be preserved, not
// overwritten by our own idea of the total.
func TestFlushMergesForeignIncrements(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	const repo = "/tmp/merge-repo"
	tr := NewTracker(repo)
	tr.SetStartTime(time.Now())
	defer tr.Close()

	tr.OnToolCall("explore", repo) // ours: 1

	path, err := usagePath(canonicalRepoPath(repo))
	if err != nil {
		t.Fatal(err)
	}

	// Simulate another server bumping the shared file behind our back.
	info, _, err := ReadStatus(path)
	if err != nil {
		t.Fatal(err)
	}
	info.ToolCounts["explore"] += 10
	info.ToolCounts["query_facts"] = 3
	writeStatusFile(t, path, info)

	tr.OnToolCall("explore", repo) // ours: 2

	after, _, err := ReadStatus(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := after.ToolCounts["explore"]; got != 12 {
		t.Errorf("explore = %d, want 12 (1 ours + 10 sibling + 1 ours)", got)
	}
	if got := after.ToolCounts["query_facts"]; got != 3 {
		t.Errorf("query_facts = %d, want 3 — a sibling's tool counts were dropped", got)
	}
}

// TestAggregateServerPrefersRegistryForLiveState verifies --status and the
// dashboard read live process state from the registry rather than from whichever
// usage file happens to carry the newest StartTime.
func TestAggregateServerPrefersRegistryForLiveState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir, err := usageDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	// A usage file claiming a dead server started later than the live one. Under
	// the old newest-StartTime rule this would have been reported as "the server".
	writeUsage(t, dir, StatusInfo{
		RepoPath: "/repo/stale", PID: 0, StartTime: time.Now().Add(time.Hour),
		DashboardPort: 9999, ToolCounts: map[string]int{"explore": 4},
	})

	tr := NewTracker("/repo/live")
	tr.SetStartTime(time.Now())
	tr.SetDashboardPort(4321)
	tr.PersistStartup()
	defer tr.Close()

	ss := AggregateServer(dir)
	if ss.PID != os.Getpid() {
		t.Errorf("PID = %d, want %d (the live registered server)", ss.PID, os.Getpid())
	}
	if ss.DashboardPort != 4321 {
		t.Errorf("DashboardPort = %d, want 4321 (not the dead server's 9999)", ss.DashboardPort)
	}
	if !ss.Alive {
		t.Error("Alive = false, want true")
	}
	if len(ss.Instances) != 1 {
		t.Errorf("Instances = %d, want 1", len(ss.Instances))
	}
}

// writeStatusFile writes a StatusInfo to an explicit path, standing in for
// another enola process updating the shared per-repo counter file.
func writeStatusFile(t *testing.T, path string, info StatusInfo) {
	t.Helper()
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
