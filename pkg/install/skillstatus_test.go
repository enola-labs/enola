package install

import (
	"os"
	"path/filepath"
	"testing"
)

func states(t *testing.T, loc SkillsLocation) map[string]SkillState {
	t.Helper()
	out := map[string]SkillState{}
	for _, s := range loc.Skills {
		out[s.Name] = s.State
	}
	return out
}

// Every state doctor reports, from one repository: what install wrote is current;
// a file changed since is outdated; one deleted is missing; one a newer version stopped
// shipping is retired; a directory with enola's name and someone else's skill is theirs.
func TestSkillsReportStates(t *testing.T) {
	o := opts(t, false)
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	locs, err := SkillsReport(o.RepoDir, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(locs) != 2 {
		t.Fatalf("locations: %d, want the repository's two", len(locs))
	}
	for _, l := range locs {
		for name, st := range states(t, l) {
			if st != SkillCurrent {
				t.Errorf("%s/%s right after install: %s", l.Dir, name, st)
			}
		}
	}
	if _, ok := states(t, locs[0])["enola"]; ok {
		t.Error("Claude Code's directory lists the routing skill it does not get")
	}

	agents := filepath.Join(o.RepoDir, ".agents", "skills")
	write := func(rel, content string) {
		p := filepath.Join(agents, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	md := read(t, filepath.Join(agents, "enola-review", "SKILL.md"))
	write("enola-review/SKILL.md", md+"\nedited\n")
	if err := os.RemoveAll(filepath.Join(agents, "enola-memory")); err != nil {
		t.Fatal(err)
	}
	write("enola-retired/SKILL.md", "---\nname: enola-retired\n---\n\n"+skillMarker)
	if err := os.RemoveAll(filepath.Join(agents, "enola-intent")); err != nil {
		t.Fatal(err)
	}
	write("enola-intent/SKILL.md", "---\nname: enola-intent\ndescription: mine\n---\nmine\n")

	locs, err = SkillsReport(o.RepoDir, "")
	if err != nil {
		t.Fatal(err)
	}
	got := states(t, locs[1])
	for name, want := range map[string]SkillState{
		"enola-review": SkillOutdated, "enola-memory": SkillMissing, "enola-retired": SkillRetired,
		"enola-intent": SkillTheirs, "enola-test-plan": SkillCurrent,
	} {
		if got[name] != want {
			t.Errorf("%s: %q, want %q", name, got[name], want)
		}
	}
}

// With the plugin serving Claude Code, its directory is expected to hold none of
// enola's skills: one an older install left there is reported retired.
func TestSkillsReportPluginServes(t *testing.T) {
	o := opts(t, false)
	o.Targets = []string{"claude"}
	if _, err := Install(o); err != nil {
		t.Fatal(err)
	}
	reg := filepath.Join(o.HomeDir, ".claude", "plugins", "installed_plugins.json")
	if err := os.MkdirAll(filepath.Dir(reg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reg, []byte(`{"version":2,"plugins":{"enola@enola":[{"scope":"user"}]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	locs, err := SkillsReport(o.RepoDir, o.HomeDir)
	if err != nil {
		t.Fatal(err)
	}
	if !locs[0].ServedByPlugin {
		t.Fatal("the plugin was not noticed")
	}
	for name, st := range states(t, locs[0]) {
		if st != SkillRetired {
			t.Errorf("%s under a plugin: %s, want retired", name, st)
		}
	}
}
