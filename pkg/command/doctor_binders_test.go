package command

import "testing"

// Doctor names how many binders this binary runs on every snapshot, by stage, and
// where a snapshot records the ones that ran: the receipt is the only place a binder
// that failed leaves a trace after the run.
func TestDoctor_BinderLineCountsByStage(t *testing.T) {
	got := binderLine([]string{"a"}, []string{"b", "c"})
	want := "binders: 3 run on every snapshot (1 pre-link, 2 post-link); a snapshot's receipt lists those that ran without error"
	if got != want {
		t.Errorf("binder line:\n got %q\nwant %q", got, want)
	}
}
