package tsextractor

import (
	"slices"
	"testing"

	"github.com/enola-labs/enola/internal/facts"
)

// An MCP tool registered in TypeScript is an entry named by the tool: an inline
// handler gets a symbol "<dir>.tool:<name>" with its calls, a named one carries
// the tool, through the SDK's server.tool and registerTool and through a wrapper of
// the project's own.
func TestMCPTools_RegisteredHandlers(t *testing.T) {
	ff := extractAll(t, map[string]string{
		"src/tools/candidates.ts": `import { McpServer } from '@modelcontextprotocol/sdk/server/mcp.js';
import { registerTool } from './tool-helper';
import { apiClient } from '../api-client';

export const registerCandidateTools = (server: McpServer) => {
	registerTool(server, 'get_candidates', 'List candidates', {}, async (args) => {
		return apiClient.get('/candidates', args);
	});
	server.tool('get_candidate_count', 'Count', {}, async () => {
		return apiClient.count();
	});
	server.registerTool('archive_candidate', { description: 'Archive' }, archiveCandidate);
};

async function archiveCandidate(args: any) {
	return apiClient.archive(args);
}
`,
		"src/tools/tool-helper.ts": `import { McpServer } from '@modelcontextprotocol/sdk/server/mcp.js';

export function registerTool(server: McpServer, name: string, description: string, schema: any, callback: any) {
	(server.registerTool as any)(name, { description }, callback);
}
`,
		"src/api-client.ts": `export const apiClient = {
	get: (p: string, a: any) => fetch(p),
	count: () => fetch('/count'),
	archive: (a: any) => fetch('/archive'),
};
`,
		"src/other.ts": `const cli = { tool: (n: string, f: any) => f };
cli.tool('not_mcp', () => 1);
`,
	}, false)
	tools := map[string]string{}
	calls := map[string][]string{}
	for _, f := range ff {
		if f.Kind != facts.KindSymbol || f.PropString("tool") == "" {
			continue
		}
		tools[f.PropString("tool")] = f.Name
		if !f.PropBool("framework_registered") {
			t.Errorf("%s is not framework_registered", f.Name)
		}
		for _, r := range f.Relations {
			if r.Kind == facts.RelCalls {
				calls[f.Name] = append(calls[f.Name], r.Target)
			}
		}
	}
	want := map[string]string{
		"get_candidates":      "src/tools.tool:get_candidates",
		"get_candidate_count": "src/tools.tool:get_candidate_count",
		"archive_candidate":   "src/tools.archiveCandidate",
	}
	for tool, sym := range want {
		if tools[tool] != sym {
			t.Errorf("tool %s = %q, want %q (have %v)", tool, tools[tool], sym, tools)
		}
	}
	if len(tools) != len(want) {
		t.Errorf("tools = %v, want exactly %v", tools, want)
	}
	if !slices.Contains(calls["src/tools.tool:get_candidates"], "src.apiClient.get") {
		t.Errorf("get_candidates calls %v, want src.apiClient.get", calls["src/tools.tool:get_candidates"])
	}
}
