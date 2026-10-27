package server

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/enola-labs/enola/internal/locate"
)

type locateArgs struct {
	Locations []string       `json:"locations,omitempty" jsonschema:"file:line or file:a-b"`
	Diff      string         `json:"diff,omitempty" jsonschema:"unified diff"`
	Side      string         `json:"side,omitempty" jsonschema:"diff side the snapshot is of: new (default) or old"`
	Stamps    []locate.Stamp `json:"stamps,omitempty"`
}

// maxLocateInputs caps the locations and the stamps one call may carry.
const maxLocateInputs = 500

// registerLocateTool registers locate, the conversion between places in source and fact
// ids. It is the call an integration makes to key what it remembers about code on
// something that survives the code moving.
func (s *Server) registerLocateTool() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "locate",
		Description: "Source places to symbols and back. locations (file:line) or a git diff give the innermost symbols with fact id and body_hash, the version of their code. " +
			"stamps [{id, body_hash, name}] come back current, rewritten, moved or gone.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, args locateArgs) (*mcp.CallToolResult, any, error) {
		if len(args.Locations) == 0 && args.Diff == "" && len(args.Stamps) == 0 {
			return errorResult("locate needs locations, a diff or stamps"), nil, nil
		}
		if len(args.Locations) > maxLocateInputs || len(args.Stamps) > maxLocateInputs {
			return errorResult(fmt.Sprintf("at most %d locations and %d stamps per call", maxLocateInputs, maxLocateInputs)), nil, nil
		}
		store := s.eng.Store()
		if store.Count() == 0 {
			return errorResult("No facts available. Run generate_snapshot first."), nil, nil
		}
		// FactsRef: NewIndex copies the few fields it keeps and neither retains nor
		// mutates the slice.
		ix := locate.NewIndex(store.FactsRef())

		var out struct {
			Locations []locate.LocationResult `json:"locations,omitempty"`
			Diff      *locate.DiffResult      `json:"diff,omitempty"`
			Stamps    []locate.CheckResult    `json:"stamps,omitempty"`
		}
		if len(args.Locations) > 0 {
			out.Locations = ix.Locations(args.Locations)
		}
		if args.Diff != "" {
			d, err := ix.Diff(args.Diff, args.Side)
			if err != nil {
				return errorResult(err.Error()), nil, nil
			}
			out.Diff = &d
		}
		if len(args.Stamps) > 0 {
			out.Stamps = ix.Check(args.Stamps)
		}
		return jsonResult(out)
	})
}
