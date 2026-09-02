package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/enola-labs/enola/pkg/facts"
	"github.com/enola-labs/enola/pkg/status"
)

// TestPageDescribesItsOwnServer guards the Activity tab: runtime identity comes
// from the tracker for this process, never from a sibling server.
func TestPageDescribesItsOwnServer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	const foreignPID = 999999 // never a live PID, so it must not be picked
	registerForeign(t, status.Instance{
		PID:           foreignPID,
		StartTime:     time.Now().Add(time.Hour),
		Heartbeat:     time.Now().Add(time.Hour),
		Binary:        "enola",
		DashboardPort: 59999,
	})

	tr := status.NewTracker("/tmp/my-repo")
	tr.SetStartTime(time.Now().Add(-5 * time.Minute))
	tr.SetIdentity(status.Identity{Binary: "enola-enterprise", Version: "4.2.0", WorkDir: "/tmp/my-workspace"})
	tr.SetGraphFunc(func() status.GraphState {
		return status.GraphState{Repos: []status.InstanceRepo{{Label: "my-repo", Path: "/tmp/my-repo"}}}
	})
	tr.PersistStartup()
	defer tr.Close()

	s := newTestServer(8080, fakeArtifacts{}, Options{Tracker: tr})
	rec := httptest.NewRecorder()
	s.handleIndex(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()

	for _, operational := range []string{"panel-activity", "enola-enterprise 4.2.0", "/tmp/my-workspace", "my-repo"} {
		if !strings.Contains(body, operational) {
			t.Errorf("Activity tab missing this server's operational detail %q", operational)
		}
	}
}

// TestPageListsLiveInstances confirms the Activity tab provides the original
// server switcher alongside the product-focused Overview and Architecture tabs.
func TestPageListsLiveInstances(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// A sibling that is genuinely alive (this test's own process, under a start
	// time far enough back to be a distinct record) would collide with the
	// tracker's record, so use the parent process: alive, and not us.
	sibling := os.Getppid()
	registerForeign(t, status.Instance{
		PID:           sibling,
		StartTime:     time.Now().Add(-time.Minute),
		Heartbeat:     time.Now(),
		Binary:        "enola",
		DashboardPort: 54545,
		FrontDoor:     true,
		Repos:         []status.InstanceRepo{{Label: "other-repo", Path: "/tmp/other"}},
	})

	tr := status.NewTracker("/tmp/my-repo")
	tr.SetStartTime(time.Now())
	tr.SetDashboardPort(8080)
	tr.SetIdentity(status.Identity{Binary: "enola"})
	tr.PersistStartup()
	defer tr.Close()

	s := newTestServer(8080, fakeArtifacts{}, Options{Tracker: tr})
	rec := httptest.NewRecorder()
	s.handleIndex(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()

	for _, operational := range []string{"Active sessions", "http://127.0.0.1:54545", "other-repo", "this page"} {
		if !strings.Contains(body, operational) {
			t.Errorf("Activity tab missing server-switcher detail %q", operational)
		}
	}
	activeAt := strings.Index(body, "Active sessions")
	runtimeAt := strings.Index(body, "Paths and runtime details")
	if activeAt < 0 || runtimeAt < 0 || activeAt > runtimeAt {
		t.Error("Diagnostics must show active sessions before collapsed runtime details")
	}
	if !strings.Contains(body, "These totals do not affect the snapshot shown elsewhere") {
		t.Error("Lifetime usage must be clearly separated from the current snapshot")
	}
}

// TestPageTracksInstancesJoiningAndLeaving exercises the lifecycle a user gets
// from opening and closing agent tabs. The dashboard rebuilds its page model on
// every request, so a sibling must appear without restarting the front door and
// disappear again as soon as its registry record is removed.
func TestPageTracksInstancesJoiningAndLeaving(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	tr := status.NewTracker("/tmp/my-repo")
	tr.SetStartTime(time.Now())
	tr.SetDashboardPort(8080)
	tr.SetIdentity(status.Identity{Binary: "enola"})
	tr.PersistStartup()
	defer tr.Close()

	s := newTestServer(8080, fakeArtifacts{}, Options{Tracker: tr})
	render := func() string {
		t.Helper()
		rec := httptest.NewRecorder()
		s.handleIndex(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		return rec.Body.String()
	}

	if body := render(); strings.Contains(body, "joining-repo") {
		t.Fatal("sibling is visible before it starts")
	}

	sibling := status.Instance{
		PID:           os.Getppid(),
		StartTime:     time.Now().Add(-time.Minute),
		Heartbeat:     time.Now(),
		Binary:        "enola",
		DashboardPort: 54545,
		Repos:         []status.InstanceRepo{{Label: "joining-repo", Path: "/tmp/joining"}},
	}
	registerForeign(t, sibling)
	if body := render(); !strings.Contains(body, "joining-repo") ||
		!strings.Contains(body, "http://127.0.0.1:54545") {
		t.Fatal("new sibling did not appear on the next dashboard request")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(home, ".enola", "instances",
		fmt.Sprintf("%d-%d.json", sibling.PID, sibling.StartTime.UnixNano()))
	if err := os.Remove(record); err != nil {
		t.Fatal(err)
	}
	if body := render(); strings.Contains(body, "joining-repo") ||
		strings.Contains(body, "http://127.0.0.1:54545") {
		t.Fatal("departed sibling remained visible on the next dashboard request")
	}
}

// TestPageWithoutTrackerStillRenders guards the zero-Options path: a caller that
// supplies no tracker (as the tests and any minimal embedder do) must still get a
// page rather than a nil dereference.
func TestPageWithoutTrackerStillRenders(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	s := newTestServer(8080, fakeArtifacts{graph: &facts.GraphReceipt{SnapshotID: "g1"}})
	rec := httptest.NewRecorder()
	s.handleIndex(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Your architecture is mapped.") {
		t.Error("body missing the populated architecture state")
	}
	for _, tab := range []string{"overview", "architecture", "snapshots", "activity", "quality"} {
		if !strings.Contains(rec.Body.String(), `id="tab-`+tab+`"`) ||
			!strings.Contains(rec.Body.String(), `id="panel-`+tab+`"`) {
			t.Errorf("body missing %q tab or panel", tab)
		}
	}
	if body := rec.Body.String(); !strings.Contains(body, `href="https://github.com/enola-labs/enola"`) ||
		!strings.Contains(body, "Open source on GitHub · Star Enola") {
		t.Error("body missing the subtle open-source project link")
	}
}

func TestResolveStablePort(t *testing.T) {
	tests := []struct {
		name       string
		env        string
		setEnv     bool
		configured int
		want       int
	}{
		{name: "default when unset", want: DefaultStablePort},
		{name: "configured port wins over default", configured: 9000, want: 9000},
		{name: "negative config disables", configured: -1, want: 0},
		{name: "env overrides config", env: "9100", setEnv: true, configured: 9000, want: 9100},
		{name: "env off disables", env: "off", setEnv: true, configured: 9000, want: 0},
		{name: "env zero disables", env: "0", setEnv: true, want: 0},
		{name: "unparseable env falls back to config", env: "banana", setEnv: true, configured: 9000, want: 9000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setEnv {
				t.Setenv(StablePortEnv, tt.env)
			} else if err := os.Unsetenv(StablePortEnv); err != nil {
				t.Fatal(err)
			}
			if got := ResolveStablePort(tt.configured); got != tt.want {
				t.Errorf("ResolveStablePort(%d) = %d, want %d", tt.configured, got, tt.want)
			}
		})
	}
}

// registerForeign plants another server's record in the isolated registry,
// standing in for a second agent terminal's enola process. It writes the file
// directly — the on-disk layout is the registry's cross-process contract, and
// pkg/status offers no way for one process to register another.
func registerForeign(t *testing.T, inst status.Instance) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".enola", "instances")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(inst)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("%d-%d.json", inst.PID, inst.StartTime.UnixNano())
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
}
