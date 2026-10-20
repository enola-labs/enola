package server

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/enola-labs/enola/internal/engine"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// withProgress returns ctx carrying a reporter that turns a snapshot's
// extraction progress into progress notifications to the session that made
// the request, when the request carried a progress token; otherwise ctx
// unchanged, and nothing is sent.
//
// MCP requires the progress to increase with each notification. A cluster
// generates one repository after another and each starts its count again, so
// each repository's files are counted on from where the one before it ended,
// and the total grows as each repository's walk finds its files.
func withProgress(ctx context.Context, req *mcp.CallToolRequest) context.Context {
	if req == nil || req.Params == nil || req.Session == nil {
		return ctx
	}
	token := req.Params.GetProgressToken()
	if token == nil {
		return ctx
	}
	var (
		mu          sync.Mutex
		repo        string
		base, found float64
		last        float64
		sent        bool
	)
	return engine.WithProgress(ctx, func(p engine.Progress) {
		mu.Lock()
		defer mu.Unlock()
		if p.Repo != repo {
			base += found
			repo = p.Repo
		}
		found = float64(p.Found)
		done := base + float64(p.Extracted)
		if base+found == 0 || (sent && done <= last) {
			return
		}
		sent, last = true, done
		msg := fmt.Sprintf("%s: %d of %d files extracted", filepath.Base(p.Repo), p.Extracted, p.Found)
		if p.Extractor != "" {
			msg += " (" + p.Extractor + " finished)"
		}
		_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
			ProgressToken: token,
			Progress:      done,
			Total:         base + found,
			Message:       msg,
		})
	})
}
