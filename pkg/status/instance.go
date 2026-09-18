package status

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// An enola MCP server is launched once per agent/terminal session, so a user
// commonly has several running at the same time — different repos, different
// binaries (enola, or a wrapper built on it), each with its own in-memory graph and its
// own dashboard on its own ephemeral port.
//
// The instance registry is what makes that fleet observable. Every server writes
// one record describing itself to
//
//	~/.enola/instances/<pid>-<startUnixNano>.json
//
// and removes it on exit. Each file has exactly one writer (its own process), so
// unlike the per-repo usage files there is no write contention and no need for a
// lock. Readers (LiveInstances) reap records whose process is gone, so a hard
// kill self-heals.
//
// The start-time suffix defeats PID reuse: a recycled PID cannot resurrect a
// dead instance's record.

// instancesDirName is the registry directory under ~/.enola.
const instancesDirName = "instances"

// heartbeatInterval is how often a running server refreshes its record so an
// idle instance (no tool calls) still looks alive to readers.
const heartbeatInterval = 30 * time.Second

// staleAfter bounds how long a record with a live PID may go un-refreshed before
// readers treat it as dead. It is a backstop for a PID that was reused by an
// unrelated process between the write and the read; generously above
// heartbeatInterval so a busy or paused server is never reaped by mistake.
const staleAfter = 10 * time.Minute

// InstanceRepo is one repository loaded into an instance's graph.
type InstanceRepo struct {
	Label     string `json:"label"`
	Path      string `json:"path"`
	FactCount int    `json:"fact_count,omitempty"`
}

// Instance is one running enola MCP server, as it describes itself on disk. It
// answers the question a user with several agent terminals open cannot otherwise
// answer: which process is this, what has it loaded, and where is its dashboard.
type Instance struct {
	PID       int       `json:"pid"`
	StartTime time.Time `json:"start_time"`
	Heartbeat time.Time `json:"heartbeat"`

	// Identity of the binary and the launch context — what distinguishes two
	// instances at a glance.
	Binary     string `json:"binary"`                // the binary's own name
	Version    string `json:"version,omitempty"`     // build version
	ConfigPath string `json:"config_path,omitempty"` // resolved mcp-arch.yaml
	WorkDir    string `json:"work_dir,omitempty"`    // cwd, i.e. which workspace launched it

	// Graph state, refreshed after each generate_snapshot.
	PrimaryRepo  string         `json:"primary_repo,omitempty"` // abs cfg.Repo
	Repos        []InstanceRepo `json:"repos,omitempty"`
	SnapshotID   string         `json:"snapshot_id,omitempty"`
	SnapshotAt   time.Time      `json:"snapshot_at,omitempty"`
	FactCount    int            `json:"fact_count,omitempty"`
	InsightCount int            `json:"insight_count,omitempty"`

	// Dashboard reachability. FrontDoor marks the instance that currently owns
	// the stable, bookmarkable port (see pkg/dashboard).
	DashboardPort int  `json:"dashboard_port,omitempty"`
	FrontDoor     bool `json:"front_door,omitempty"`

	// This process's own activity — never aggregated from other instances.
	LastTool      string         `json:"last_tool,omitempty"`
	LastCallAt    time.Time      `json:"last_call_at,omitempty"`
	SessionCounts map[string]int `json:"session_counts,omitempty"`
	SessionTokens map[string]int `json:"session_tokens,omitempty"` // token credit earned this session, per tool
}

// URL is the instance's dashboard URL, or "" when it serves no dashboard.
func (i Instance) URL() string {
	if i.DashboardPort <= 0 {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d", i.DashboardPort)
}

// SessionCalls is the total number of tool calls this instance has served.
func (i Instance) SessionCalls() int {
	n := 0
	for _, v := range i.SessionCounts {
		n += v
	}
	return n
}

// RepoLabels renders the instance's loaded repos as a compact "a, b, c" label,
// falling back to the primary repo's base name when no graph is loaded yet.
func (i Instance) RepoLabels() string {
	if len(i.Repos) == 0 {
		if i.PrimaryRepo == "" {
			return ""
		}
		return filepath.Base(i.PrimaryRepo)
	}
	labels := make([]string, 0, len(i.Repos))
	for _, r := range i.Repos {
		labels = append(labels, r.Label)
	}
	return strings.Join(labels, ", ")
}

// instancesDir returns the directory holding per-instance records.
func instancesDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".enola", instancesDirName), nil
}

// instanceFileName is the record's filename: PID plus start time, so a reused
// PID cannot collide with a dead instance's record.
func instanceFileName(pid int, start time.Time) string {
	return fmt.Sprintf("%d-%d.json", pid, start.UnixNano())
}

// writeInstance persists one instance record, best-effort. The file has a single
// writer (the process it describes), so a plain write is safe; it is still
// written atomically so a concurrent reader never sees a truncated record.
func writeInstance(inst Instance) error {
	dir, err := instancesDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(inst, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, instanceFileName(inst.PID, inst.StartTime)), data, 0o644)
}

// removeInstance deletes one instance's record. Best-effort: a failure just
// leaves a stale record for LiveInstances to reap.
func removeInstance(pid int, start time.Time) {
	dir, err := instancesDir()
	if err != nil {
		return
	}
	_ = os.Remove(filepath.Join(dir, instanceFileName(pid, start)))
}

// LiveInstances returns every enola MCP server currently running on this
// machine for this user, newest last, and reaps the records of those that are
// not. A record is considered dead when its PID is gone, or when its PID is
// alive but the record has not been refreshed for staleAfter (a PID-reuse
// guard). Best-effort throughout: unreadable or malformed records are skipped.
func LiveInstances() []Instance {
	dir, err := instancesDir()
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	now := time.Now()
	var live []Instance
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var inst Instance
		if err := json.Unmarshal(data, &inst); err != nil {
			// A malformed record can never become valid; drop it.
			_ = os.Remove(path)
			continue
		}
		if !instanceAlive(inst, now) {
			_ = os.Remove(path)
			continue
		}
		live = append(live, inst)
	}

	sort.Slice(live, func(i, j int) bool {
		if live[i].StartTime.Equal(live[j].StartTime) {
			return live[i].PID < live[j].PID
		}
		return live[i].StartTime.Before(live[j].StartTime)
	})
	return live
}

// instanceAlive reports whether a record describes a still-running server. The
// current process is always alive regardless of heartbeat age, so a server that
// reads the registry before its first heartbeat never reaps itself.
func instanceAlive(inst Instance, now time.Time) bool {
	if inst.PID == os.Getpid() {
		return true
	}
	if !isProcessAlive(inst.PID) {
		return false
	}
	hb := inst.Heartbeat
	if hb.IsZero() {
		hb = inst.StartTime
	}
	return now.Sub(hb) <= staleAfter
}
