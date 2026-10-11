package engine_test

// The order the engine drives its plugins in, and what each one can see when it
// runs, is a contract no signature states: a binder reads the facts every
// extractor emitted, a post-link binder reads what linking added, an explainer
// reads a bound graph, a renderer reads the findings. Nothing fails to compile
// when a stage moves; a binder just starts resolving against a store that does
// not hold its inputs yet, and the snapshot comes out thinner.
//
// These tests register recording plugins and pin that order and that visibility,
// so GenerateSnapshot can be taken apart into stages and put back without the
// order changing silently.

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/enola-labs/enola/internal/config"
	"github.com/enola-labs/enola/internal/engine"
	"github.com/enola-labs/enola/internal/facts"
	"github.com/enola-labs/enola/pkg/plugin"
)

// stageLog records what ran, in order.
type stageLog struct {
	mu     sync.Mutex
	events []string
}

func (l *stageLog) add(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *stageLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

// hasFact reports whether the store holds a fact of that kind and name.
func hasFact(store *facts.Store, kind, name string) bool {
	for _, f := range store.ByKind(kind) {
		if f.Name == name {
			return true
		}
	}
	return false
}

type recExtractor struct {
	log *stageLog
	err error
}

func (x *recExtractor) Name() string                { return "rec" }
func (x *recExtractor) Detect(string) (bool, error) { x.log.add("detect"); return true, nil }
func (x *recExtractor) Extract(_ context.Context, _ string, _ []string) ([]facts.Fact, error) {
	x.log.add("extract")
	if x.err != nil {
		return nil, x.err
	}
	return []facts.Fact{{Kind: facts.KindModule, Name: "extracted", File: "a.rec"}}, nil
}

type recBinder struct {
	log   *stageLog
	stage plugin.BindStage
	err   error
	// sawExtracted and sawPreLink are what the store held when this binder ran.
	sawExtracted, sawPreLink, sawFrozen bool
}

func (b *recBinder) Name() string            { return "rec-" + string(b.stage) }
func (b *recBinder) Stage() plugin.BindStage { return b.stage }
func (b *recBinder) Bind(_ context.Context, store *facts.Store) error {
	b.log.add("bind:" + string(b.stage))
	b.sawExtracted = hasFact(store, facts.KindModule, "extracted")
	b.sawPreLink = hasFact(store, facts.KindModule, "bound-pre-link")
	b.sawFrozen = store.Frozen()
	if b.err != nil {
		return b.err
	}
	store.Add(facts.Fact{Kind: facts.KindModule, Name: "bound-" + string(b.stage), File: "a.rec"})
	return nil
}

type recExplainer struct {
	log *stageLog
	err error
	// sawAnnotated is whether Annotate had run by the time Explain did.
	annotated, sawAnnotated      bool
	sawPreLink, sawPostLink      bool
	sawFrozenAtAnnotate, sawFroz bool
}

func (e *recExplainer) Name() string { return "rec" }
func (e *recExplainer) Annotate(_ context.Context, store *facts.Store) error {
	e.log.add("annotate")
	e.annotated = true
	e.sawFrozenAtAnnotate = store.Frozen()
	return nil
}
func (e *recExplainer) Explain(_ context.Context, store *facts.Store) ([]facts.Insight, error) {
	e.log.add("explain")
	e.sawAnnotated = e.annotated
	e.sawPreLink = hasFact(store, facts.KindModule, "bound-pre-link")
	e.sawPostLink = hasFact(store, facts.KindModule, "bound-post-link")
	e.sawFroz = store.Frozen()
	if e.err != nil {
		return nil, e.err
	}
	return []facts.Insight{{Title: "recorded finding", Confidence: 1}}, nil
}

type recRenderer struct {
	log         *stageLog
	sawInsights []facts.Insight
	sawFacts    int
}

func (r *recRenderer) Name() string { return "rec" }
func (r *recRenderer) Render(_ context.Context, snap *facts.Snapshot) ([]facts.Artifact, error) {
	r.log.add("render")
	r.sawInsights = append([]facts.Insight(nil), snap.Insights...)
	r.sawFacts = len(snap.Facts)
	return []facts.Artifact{{Name: "rec.txt", Content: []byte("rendered"), Type: "text/plain"}}, nil
}

type recPlugins struct {
	log       *stageLog
	extractor *recExtractor
	pre, post *recBinder
	explainer *recExplainer
	renderer  *recRenderer
}

// recordingEngine builds an engine that runs only the recording plugins, over a
// repository holding one file for the extractor to be handed.
func recordingEngine(t *testing.T) (*engine.Engine, *recPlugins, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, "a.rec"), "content\n")

	cfg := config.Default()
	cfg.Extractors = []string{"rec"}
	cfg.Explainers = []string{"rec"}
	cfg.Renderers = []string{"rec"}
	eng, err := engine.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	log := &stageLog{}
	p := &recPlugins{
		log:       log,
		extractor: &recExtractor{log: log},
		pre:       &recBinder{log: log, stage: plugin.StagePreLink},
		post:      &recBinder{log: log, stage: plugin.StagePostLink},
		explainer: &recExplainer{log: log},
		renderer:  &recRenderer{log: log},
	}
	eng.RegisterExtractor(p.extractor)
	// Registered post-link first: the stage a binder declares decides when it
	// runs, not the order it was registered in.
	eng.RegisterBinder(p.post)
	eng.RegisterBinder(p.pre)
	eng.RegisterExplainer(p.explainer)
	eng.RegisterRenderer(p.renderer)
	return eng, p, repo
}

func TestPipeline_StagesRunInOrder(t *testing.T) {
	eng, p, repo := recordingEngine(t)

	snap, err := eng.GenerateSnapshot(context.Background(), repo, false)
	if err != nil {
		t.Fatalf("GenerateSnapshot: %v", err)
	}

	want := []string{"detect", "extract", "bind:pre-link", "bind:post-link", "annotate", "explain", "render"}
	if got := p.log.snapshot(); !reflect.DeepEqual(got, want) {
		t.Errorf("stage order:\n got %v\nwant %v", got, want)
	}

	if !p.pre.sawExtracted {
		t.Error("the pre-link binder ran before the extractor's facts were in the store")
	}
	if !p.post.sawExtracted || !p.post.sawPreLink {
		t.Errorf("the post-link binder must see extracted and pre-link facts; saw extracted=%v pre-link=%v",
			p.post.sawExtracted, p.post.sawPreLink)
	}
	if !p.explainer.sawAnnotated {
		t.Error("Explain ran before Annotate")
	}
	if !p.explainer.sawPreLink || !p.explainer.sawPostLink {
		t.Errorf("the explainer must see a fully bound store; saw pre-link=%v post-link=%v",
			p.explainer.sawPreLink, p.explainer.sawPostLink)
	}

	// Binders and annotators write to the store, so it cannot be frozen under them.
	if p.pre.sawFrozen || p.post.sawFrozen || p.explainer.sawFrozenAtAnnotate {
		t.Errorf("the store was frozen while a stage that writes to it ran: pre=%v post=%v annotate=%v",
			p.pre.sawFrozen, p.post.sawFrozen, p.explainer.sawFrozenAtAnnotate)
	}
	// Published means frozen: readers never see a store a stage can still write to.
	if !eng.Store().Frozen() {
		t.Error("the published store is not frozen")
	}

	if len(p.renderer.sawInsights) != 1 || p.renderer.sawInsights[0].Title != "recorded finding" {
		t.Errorf("the renderer must see the explainer's findings; saw %+v", p.renderer.sawInsights)
	}
	// The engine stamps a finding with the explainer that raised it.
	if got := p.renderer.sawInsights[0].Source; got != "rec" {
		t.Errorf("finding source = %q, want the explainer's name", got)
	}
	if p.renderer.sawFacts != len(snap.Facts) || p.renderer.sawFacts == 0 {
		t.Errorf("the renderer saw %d facts, the snapshot holds %d", p.renderer.sawFacts, len(snap.Facts))
	}

	for _, name := range []string{"extracted", "bound-pre-link", "bound-post-link"} {
		if !hasFact(eng.Store(), facts.KindModule, name) {
			t.Errorf("published store is missing %q", name)
		}
	}
	if len(snap.Artifacts) != 1 || snap.Artifacts[0].Name != "rec.txt" {
		t.Errorf("snapshot artifacts = %+v, want the renderer's one", snap.Artifacts)
	}
	if got := snap.Meta.Extractors; !reflect.DeepEqual(got, []string{"rec"}) {
		t.Errorf("meta extractors = %v, want [rec]", got)
	}
	if got := snap.Meta.Explainers; !reflect.DeepEqual(got, []string{"rec"}) {
		t.Errorf("meta explainers = %v, want [rec]", got)
	}
}

// A second run repeats the whole sequence: no stage is skipped because a
// snapshot is already loaded.
func TestPipeline_SecondRunRepeatsEveryStage(t *testing.T) {
	eng, p, repo := recordingEngine(t)
	ctx := context.Background()
	if _, err := eng.GenerateSnapshot(ctx, repo, false); err != nil {
		t.Fatal(err)
	}
	first := eng.Store()
	if _, err := eng.GenerateSnapshot(ctx, repo, false); err != nil {
		t.Fatal(err)
	}

	once := []string{"detect", "extract", "bind:pre-link", "bind:post-link", "annotate", "explain", "render"}
	if got, want := p.log.snapshot(), append(append([]string{}, once...), once...); !reflect.DeepEqual(got, want) {
		t.Errorf("two runs:\n got %v\nwant %v", got, want)
	}
	// A reader holding the first store keeps a consistent view of it.
	if eng.Store() == first {
		t.Error("the second run published the same store instead of a new one")
	}
	if !first.Frozen() || !hasFact(first, facts.KindModule, "extracted") {
		t.Error("the store published by the first run was disturbed by the second")
	}
}

// Deferred linking stops after extraction: the cluster walk links once at the
// end, so nothing downstream of extraction may run per repository.
func TestPipeline_DeferredLinkingStopsAfterExtraction(t *testing.T) {
	eng, p, repo := recordingEngine(t)
	eng.SetDeferLinking(true)

	if _, err := eng.GenerateSnapshot(context.Background(), repo, false); err != nil {
		t.Fatalf("GenerateSnapshot: %v", err)
	}
	if got, want := p.log.snapshot(), []string{"detect", "extract"}; !reflect.DeepEqual(got, want) {
		t.Errorf("deferred run:\n got %v\nwant %v", got, want)
	}
	if !eng.Store().Frozen() || !hasFact(eng.Store(), facts.KindModule, "extracted") {
		t.Error("a deferred run must still publish a frozen store holding the extracted facts")
	}

	eng.SetDeferLinking(false)
	if _, err := eng.GenerateSnapshot(context.Background(), repo, false); err != nil {
		t.Fatalf("GenerateSnapshot after deferral: %v", err)
	}
	want := []string{"detect", "extract", "detect", "extract", "bind:pre-link", "bind:post-link", "annotate", "explain", "render"}
	if got := p.log.snapshot(); !reflect.DeepEqual(got, want) {
		t.Errorf("run after deferral:\n got %v\nwant %v", got, want)
	}
}

// A failing binder or explainer degrades the snapshot and does not abort it: the
// stages after it still run and the facts already extracted are published.
func TestPipeline_BinderAndExplainerFailuresDoNotAbort(t *testing.T) {
	eng, p, repo := recordingEngine(t)
	p.pre.err = errors.New("pre-link binder failed")
	p.explainer.err = errors.New("explainer failed")

	snap, err := eng.GenerateSnapshot(context.Background(), repo, false)
	if err != nil {
		t.Fatalf("a failing binder or explainer must not fail the snapshot: %v", err)
	}
	want := []string{"detect", "extract", "bind:pre-link", "bind:post-link", "annotate", "explain", "render"}
	if got := p.log.snapshot(); !reflect.DeepEqual(got, want) {
		t.Errorf("stage order with failures:\n got %v\nwant %v", got, want)
	}
	if !hasFact(eng.Store(), facts.KindModule, "extracted") || !hasFact(eng.Store(), facts.KindModule, "bound-post-link") {
		t.Error("the facts from the stages that succeeded must be published")
	}
	if hasFact(eng.Store(), facts.KindModule, "bound-pre-link") {
		t.Error("the failed binder's fact was published")
	}
	if len(snap.Insights) != 0 {
		t.Errorf("a failed explainer contributes no findings; got %+v", snap.Insights)
	}
}

// A failing extractor does not abort either: its facts are missing and the run
// records the failure rather than reporting a clean extraction.
func TestPipeline_ExtractorFailureIsRecorded(t *testing.T) {
	eng, p, repo := recordingEngine(t)
	p.extractor.err = errors.New("extractor failed")

	snap, err := eng.GenerateSnapshot(context.Background(), repo, false)
	if err != nil {
		t.Fatalf("a failing extractor must not fail the snapshot: %v", err)
	}
	if hasFact(eng.Store(), facts.KindModule, "extracted") {
		t.Error("a failed extractor's fact was published")
	}
	if got := p.log.snapshot(); len(got) < 3 || got[len(got)-1] != "render" {
		t.Errorf("the stages after a failed extractor must still run; got %v", got)
	}
	if snap.Meta.ParseErrors == 0 {
		t.Error("the extractor failure is not recorded in the snapshot's parse errors")
	}
}

// A fatal extractor error is the one failure that aborts: nothing is published,
// and a reader keeps the snapshot it already had.
func TestPipeline_FatalExtractorErrorAbortsAndKeepsThePublishedSnapshot(t *testing.T) {
	eng, p, repo := recordingEngine(t)
	ctx := context.Background()
	if _, err := eng.GenerateSnapshot(ctx, repo, false); err != nil {
		t.Fatal(err)
	}
	published := eng.Store()

	p.extractor.err = plugin.Fatalf("declaration is invalid")
	if _, err := eng.GenerateSnapshot(ctx, repo, false); err == nil {
		t.Fatal("a fatal extractor error must fail the snapshot")
	}
	if eng.Store() != published {
		t.Error("a failed run replaced the published store")
	}
	once := []string{"detect", "extract", "bind:pre-link", "bind:post-link", "annotate", "explain", "render"}
	if got, want := p.log.snapshot(), append(append([]string{}, once...), "detect", "extract"); !reflect.DeepEqual(got, want) {
		t.Errorf("no stage may run after a fatal extractor error:\n got %v\nwant %v", got, want)
	}
}
