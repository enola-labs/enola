package engine

import (
	"context"

	"github.com/enola-labs/enola/pkg/plugin"
)

// Progress is how far a snapshot has got in one repository: of the files the
// walk found, how many belong to an extractor that has finished, then of the
// stages that follow extraction, how many have. Extractor is the one that just
// finished, empty for the report the walk makes. Stage names the stage that
// just finished once extraction is over; Done of Stages counts them, and
// Stages is the number this run will report, known from the walk on.
type Progress struct {
	Repo      string
	Extractor string
	Stage     string
	Extracted int
	Found     int
	Done      int
	Stages    int
}

type progressKey struct{}

// WithProgress returns a context under which a snapshot reports its progress
// to report: once when the walk has counted the files, at the end of each
// extractor that ran, and at the end of each stage after extraction. A context without one reports nothing and costs nothing,
// and what a snapshot produces never depends on whether it is listened to.
func WithProgress(ctx context.Context, report func(Progress)) context.Context {
	return context.WithValue(ctx, progressKey{}, report)
}

func progressFrom(ctx context.Context) func(Progress) {
	report, _ := ctx.Value(progressKey{}).(func(Progress))
	return report
}

// extractionProgress counts the files the finished extractors own, then the
// stages after extraction. Only an extractor that declares the files it reads
// is counted while they run: one handed every file says nothing about which it
// read, and counting them all would claim the whole repository was extracted
// the moment it finished. Files no extractor owns (a README, a test file) are
// never counted that way, so extractionDone settles them when the last
// extractor has run, and the count reaches the files found.
type extractionProgress struct {
	report  func(Progress)
	repo    string
	files   []string
	claimed []bool
	count   int
	stages  int
	done    int
}

// newExtractionProgress reports the walk's count and the number of stages that
// follow extraction, which the caller knows and a reader of the reports needs
// for a total that does not move.
func newExtractionProgress(ctx context.Context, repo string, files []string, stages int) *extractionProgress {
	report := progressFrom(ctx)
	if report == nil {
		return nil
	}
	report(Progress{Repo: repo, Found: len(files), Stages: stages})
	return &extractionProgress{report: report, repo: repo, files: files, claimed: make([]bool, len(files)), stages: stages}
}

func (p *extractionProgress) finished(ext plugin.Extractor) {
	if p == nil {
		return
	}
	if owner, ok := ext.(plugin.FileOwner); ok {
		for i, f := range p.files {
			if !p.claimed[i] && owner.OwnsFile(f) {
				p.claimed[i] = true
				p.count++
			}
		}
	}
	p.report(Progress{Repo: p.repo, Extractor: ext.Name(), Extracted: p.count, Found: len(p.files), Stages: p.stages})
}

func (p *extractionProgress) extractionDone() {
	if p == nil {
		return
	}
	p.count = len(p.files)
	p.report(Progress{Repo: p.repo, Extracted: p.count, Found: len(p.files), Stages: p.stages})
}

// stage reports that a stage after extraction has finished. A run that stops
// before announcing every stage stops short of the total, which is the truth
// about it.
func (p *extractionProgress) stage(name string) {
	if p == nil {
		return
	}
	p.done++
	p.report(Progress{Repo: p.repo, Stage: name, Extracted: p.count, Found: len(p.files), Done: p.done, Stages: p.stages})
}
