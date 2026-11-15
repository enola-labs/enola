package server

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/enola-labs/enola/internal/engine"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// progressReporter turns a snapshot's progress into progress notifications to
// the session that made the request.
//
// MCP requires the progress to increase with each notification. A cluster
// extracts several repositories at once and each counts from zero, so the
// progress is the sum of every repository's count, and the total grows as each
// repository's walk finds its files. A
// repository's stages after extraction are part of its total from the walk on,
// and so are the artifacts the handler writes afterwards once it has said how
// many, so a client sees extraction fill most of the bar, the stages after it
// move it on, and the last artifact written end it on the total.
type progressReporter struct {
	mu      sync.Mutex
	session *mcp.ServerSession
	token   any

	// found and counted are each repository's total and count, and the sums of both.
	found, counted       map[string]float64
	foundSum, countedSum float64
	tail, wrote          float64
	last                 float64
	sent                 bool
}

type progressReporterKey struct{}

// withProgress returns ctx carrying a reporter that sends progress
// notifications to the session that made the request, when the request carried
// a progress token; otherwise ctx unchanged, and nothing is sent.
func withProgress(ctx context.Context, req *mcp.CallToolRequest) context.Context {
	if req == nil || req.Params == nil || req.Session == nil {
		return ctx
	}
	token := req.Params.GetProgressToken()
	if token == nil {
		return ctx
	}
	p := &progressReporter{session: req.Session, token: token}
	ctx = context.WithValue(ctx, progressReporterKey{}, p)
	return engine.WithProgress(ctx, func(e engine.Progress) { p.engine(ctx, e) })
}

func progressReporterFrom(ctx context.Context) *progressReporter {
	p, _ := ctx.Value(progressReporterKey{}).(*progressReporter)
	return p
}

// announceArtifacts says, before the run starts, how many repositories'
// artifacts the handler will write once it is done, so the total already holds
// them. Without a token it does nothing.
func announceArtifacts(ctx context.Context, repos int) {
	p := progressReporterFrom(ctx)
	if p == nil {
		return
	}
	p.mu.Lock()
	p.tail += float64(repos)
	p.mu.Unlock()
}

// artifactsWritten reports that one repository's artifacts are on disk.
func artifactsWritten(ctx context.Context, repo string) {
	p := progressReporterFrom(ctx)
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.wrote++
	p.send(ctx, fmt.Sprintf("%s: artifacts written", filepath.Base(repo)))
}

func (p *progressReporter) engine(ctx context.Context, e engine.Progress) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.found == nil {
		p.found, p.counted = map[string]float64{}, map[string]float64{}
	}
	found, counted := float64(e.Found+e.Stages), float64(e.Extracted+e.Done)
	p.foundSum += found - p.found[e.Repo]
	p.countedSum += counted - p.counted[e.Repo]
	p.found[e.Repo], p.counted[e.Repo] = found, counted
	msg := fmt.Sprintf("%s: %d of %d files extracted", filepath.Base(e.Repo), e.Extracted, e.Found)
	switch {
	case e.Extractor != "":
		msg += " (" + e.Extractor + " finished)"
	case e.Stage != "":
		msg = fmt.Sprintf("%s: %s finished (stage %d of %d after extraction)", filepath.Base(e.Repo), e.Stage, e.Done, e.Stages)
	}
	p.send(ctx, msg)
}

// send notifies the session of where the run has got, from the counts held.
// A report that would not raise the progress, which is the walk's and an
// extractor that claimed no new file, is dropped rather than sent. Held under mu.
func (p *progressReporter) send(ctx context.Context, msg string) {
	total := p.foundSum + p.tail
	if total == 0 {
		return
	}
	done := p.countedSum + p.wrote
	if p.sent && done <= p.last {
		return
	}
	p.sent, p.last = true, done
	_ = p.session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
		ProgressToken: p.token,
		Progress:      done,
		Total:         total,
		Message:       msg,
	})
}
