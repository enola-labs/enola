package command

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/enola-labs/enola/internal/hookstate"
	"github.com/enola-labs/enola/internal/workspace"
	"github.com/enola-labs/enola/pkg/status"
)

// A session started in a folder of repositories edits repositories it did not start in,
// so neither half of the grading loop can work from the session's directory: a baseline
// of the folder would snapshot every repository in it as one, at every session start.
// Claude Code says which file each edit touches, and that is what finds the repository:
// before the session's first edit in one, the pre-edit hook pins that repository's
// baseline, and at the end of a turn the stop hook grades each repository so pinned.

// runPreEditHook pins the baseline of the repository an edit is about to land in, once
// per repository per session. It runs before the edit and waits for the snapshot, since
// a baseline taken after the edit would grade nothing. The repository's own rules apply:
// a deliberate baseline, or one another running session pinned, is left as it is.
func (r *Runner) runPreEditHook(ctx context.Context) {
	in, err := readHookInput()
	if err != nil || in.CWD == "" || in.SessionID == "" || !workspace.IsFolderOfRepos(in.CWD) {
		return
	}
	repo := repoOfEdit(in.CWD, editedPath(in.ToolInput))
	if repo == "" {
		return
	}
	if slices.Contains(loadGate(in.SessionID).Repos, repo) {
		return
	}
	r.pinBaseline(ctx, repo, status.AgentPID())
	// Loaded again: the pin took a while, and the state is not only this hook's.
	st := loadGate(in.SessionID)
	if !slices.Contains(st.Repos, repo) {
		st.Repos = append(st.Repos, repo)
		saveGate(in.SessionID, st)
	}
}

// editedPath is the file an Edit, Write, MultiEdit or NotebookEdit call names.
func editedPath(input map[string]any) string {
	for _, k := range []string{"file_path", "notebook_path"} {
		if p, ok := input[k].(string); ok && p != "" {
			return p
		}
	}
	return ""
}

// repoOfEdit is the innermost repository holding path, inside the folder and not the
// folder itself, or "" for a file outside every repository of the folder. The file need
// not exist yet: a Write creates it.
func repoOfEdit(folder, path string) string {
	if path == "" {
		return ""
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(folder, path)
	}
	folder, path = filepath.Clean(folder), filepath.Clean(path)
	for d := filepath.Dir(path); d != folder; d = filepath.Dir(d) {
		rel, err := filepath.Rel(folder, d)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			return ""
		}
		if workspace.IsRepo(d) {
			return d
		}
	}
	return ""
}

// runFolderStopHook grades every repository this session edited from the folder, each
// against its own baseline and under its own once-per-report rule, and hands back what
// they have to say together, each part naming its repository.
func (r *Runner) runFolderStopHook(ctx context.Context, in hookInput) {
	// The folder's own record, so `doctor` there can say the stop hook runs.
	hookstate.RecordFired(r.outputDirFor(in.CWD), hookstate.EventStop, hookstate.OutcomeNotARepo)
	if in.StopHookActive || in.SessionID == "" {
		return
	}
	var contexts, notices []string
	for _, repo := range loadGate(in.SessionID).Repos {
		if !isDirectory(repo) {
			continue
		}
		name := repo
		if rel, err := filepath.Rel(in.CWD, repo); err == nil {
			name = rel
		}
		c, n := r.gradeForStop(ctx, repo, in.SessionID)
		if c != "" {
			contexts = append(contexts, fmt.Sprintf("In the repository %s:\n\n%s", name, c))
		}
		if n != "" {
			notices = append(notices, fmt.Sprintf("In the repository %s: %s", name, n))
		}
	}
	if len(contexts) == 0 && len(notices) == 0 {
		return
	}
	encoded, err := json.Marshal(stopOutput(in, strings.Join(contexts, "\n\n---\n\n"), strings.Join(notices, "\n")))
	if err != nil {
		return
	}
	fmt.Println(string(encoded))
}
