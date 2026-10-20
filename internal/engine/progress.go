package engine

import (
	"context"

	"github.com/enola-labs/enola/pkg/plugin"
)

// Progress is how far a snapshot's extraction has got in one repository: of
// the files the walk found, how many belong to an extractor that has finished.
// Extractor is the one that just finished, empty for the report the walk makes.
type Progress struct {
	Repo      string
	Extractor string
	Extracted int
	Found     int
}

type progressKey struct{}

// WithProgress returns a context under which a snapshot reports its progress
// to report: once when the walk has counted the files, and at the end of each
// extractor that ran. A context without one reports nothing and costs nothing,
// and what a snapshot produces never depends on whether it is listened to.
func WithProgress(ctx context.Context, report func(Progress)) context.Context {
	return context.WithValue(ctx, progressKey{}, report)
}

func progressFrom(ctx context.Context) func(Progress) {
	report, _ := ctx.Value(progressKey{}).(func(Progress))
	return report
}

// extractionProgress counts the files the finished extractors own. Only an
// extractor that declares the files it reads is counted: one handed every file
// says nothing about which it read, and counting them all would claim the whole
// repository was extracted the moment it finished.
type extractionProgress struct {
	report  func(Progress)
	repo    string
	files   []string
	claimed []bool
	count   int
}

func newExtractionProgress(ctx context.Context, repo string, files []string) *extractionProgress {
	report := progressFrom(ctx)
	if report == nil {
		return nil
	}
	report(Progress{Repo: repo, Found: len(files)})
	return &extractionProgress{report: report, repo: repo, files: files, claimed: make([]bool, len(files))}
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
	p.report(Progress{Repo: p.repo, Extractor: ext.Name(), Extracted: p.count, Found: len(p.files)})
}
