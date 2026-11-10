package server

// Go names in text answers, shortened. A Go package is named by its import path, so
// every name in one repository repeats its module path, "github.com/acme/app/", which
// says nothing a reader of that repository needs and made text answers about a sixth
// longer. Text answers write a package as its directory instead, the form every tool
// accepts back as input, and say once which module the names are relative to.
//
// Only text: a JSON answer, and any output_mode=full, keeps every name exact, for
// consumers that key on them. A name is shortened only when the short form cannot be
// read as anything else: no other loaded Go package has the same directory (two
// repositories' internal/config stay in full), it is not a standard library package
// the code imports (a repository's own log package next to "log"), and no other fact
// is named by it. A module's root package keeps its full name; its directory is ".".

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"

	"github.com/enola-labs/enola/internal/facts"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// shortener rewrites the Go package names of one store into their directories.
type shortener struct {
	store *facts.Store
	// dirOf maps a package that may be shortened to its directory.
	dirOf map[string]string
	// moduleOf maps such a package to the module path its name starts with, for the
	// one line saying what the names are relative to.
	moduleOf map[string]string
	// taken holds the packages whose directory is another fact's name (a Go package
	// beside its cgo C sources is also a C module): the package itself stays in full,
	// its symbols are shortened.
	taken map[string]bool
}

func newShortener(store *facts.Store) *shortener {
	sh := &shortener{store: store, dirOf: map[string]string{}, moduleOf: map[string]string{}, taken: map[string]bool{}}
	if store == nil {
		return sh
	}
	byDir := map[string][]string{}
	for _, m := range store.ByKind(facts.KindModule) {
		if !facts.IsGoPackage(m) || m.File == "" || m.File == m.Name {
			continue
		}
		dir := facts.ModulePath(m)
		if dir == "" || dir == "." {
			continue // a module root keeps its name
		}
		byDir[dir] = append(byDir[dir], m.Name)
	}
	stdlib := map[string]bool{}
	for _, d := range store.ByKind(facts.KindDependency) {
		if d.PropString("source") != "stdlib" {
			continue
		}
		for _, r := range d.Relations {
			if r.Kind == facts.RelImports {
				stdlib[r.Target] = true
			}
		}
	}
	for dir, names := range byDir {
		if len(names) != 1 || stdlib[dir] {
			continue
		}
		name := names[0]
		if len(store.LookupByExactName(dir)) > 0 {
			sh.taken[name] = true
		}
		sh.dirOf[name] = dir
		sh.moduleOf[name] = strings.TrimSuffix(strings.TrimSuffix(name, dir), "/")
	}
	return sh
}

// pathChar reports a character a Go name can contain: a package path's and an
// identifier's.
func pathChar(c byte) bool {
	return c == '/' || c == '.' || c == '-' || c == '_' || c == '~' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// shorten rewrites every Go name in text that may be shortened and returns the text
// and the module paths of the names it rewrote.
func (sh *shortener) shorten(text string) (string, []string) {
	if len(sh.dirOf) == 0 || !strings.Contains(text, "/") {
		return text, nil
	}
	var b strings.Builder
	modules := map[string]bool{}
	i := 0
	for i < len(text) {
		if !pathChar(text[i]) {
			b.WriteByte(text[i])
			i++
			continue
		}
		j := i
		for j < len(text) && pathChar(text[j]) {
			j++
		}
		b.WriteString(sh.token(text[i:j], modules))
		i = j
	}
	if len(modules) == 0 {
		return text, nil
	}
	out := make([]string, 0, len(modules))
	for m := range modules {
		out = append(out, m)
	}
	sort.Strings(out)
	return b.String(), out
}

// token rewrites one run of name characters: the longest package name it starts with,
// ending where a "." or the run does, becomes that package's directory.
func (sh *shortener) token(tok string, modules map[string]bool) string {
	if !strings.Contains(tok, "/") {
		return tok
	}
	for k := len(tok); k > 0; k-- {
		if k < len(tok) && tok[k] != '.' {
			continue
		}
		dir, ok := sh.dirOf[tok[:k]]
		if !ok {
			continue
		}
		short := dir + tok[k:]
		member := strings.TrimRight(tok[k:], ".")
		// A short name some other fact carries exactly would be read back as that
		// fact: keep the full one.
		if member == "" && sh.taken[tok[:k]] {
			return tok
		}
		if member != "" && len(sh.store.LookupByExactName(dir+member)) > 0 {
			return tok
		}
		modules[sh.moduleOf[tok[:k]]] = true
		return short
	}
	return tok
}

// shortenCache holds the shortener of the store it was built for.
type shortenCache struct {
	mu    sync.Mutex
	store *facts.Store
	sh    *shortener
}

func (c *shortenCache) get(store *facts.Store) *shortener {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sh == nil || c.store != store {
		c.store, c.sh = store, newShortener(store)
	}
	return c.sh
}

// shortNamesMiddleware shortens the Go names of every text answer. Registered first,
// so innermost: the banners and notices other middleware add are not rewritten, and
// the value middleware measures the answer as it is sent.
func (s *Server) shortNamesMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		res, err := next(ctx, method, req)
		if err != nil || method != "tools/call" {
			return res, err
		}
		ctr, ok := res.(*mcp.CallToolResult)
		if !ok || ctr == nil {
			return res, err
		}
		if params, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok && wantsFullOutput(outputModeOf(params.Arguments)) {
			return res, err
		}
		store := s.loadedStore()
		if store == nil {
			return res, err
		}
		sh := s.shortNames.get(store)
		var modules []string
		for _, c := range ctr.Content {
			tc, ok := c.(*mcp.TextContent)
			if !ok {
				continue
			}
			if holdsJSON(tc.Text) {
				continue // JSON keeps exact names
			}
			text, mods := sh.shorten(tc.Text)
			if len(mods) == 0 {
				continue
			}
			tc.Text = text
			modules = append(modules, mods...)
		}
		// Last, so the answer still opens with what it is about.
		if len(modules) > 0 {
			ctr.Content = append(ctr.Content, &mcp.TextContent{Text: relativeNote(modules)})
		}
		return res, err
	}
}

// holdsJSON reports whether a text answer carries a JSON document: one that is
// JSON, or one whose JSON follows a header line ("Filters applied: …" before
// query_facts' default full output). A line opens a document when it is a lone
// bracket or starts one with a string, an object or an empty value; a Markdown
// link at a line's start does neither.
func holdsJSON(text string) bool {
	for line := range strings.Lines(text) {
		l := strings.TrimSpace(line)
		switch {
		case l == "{" || l == "[" || l == "{}" || l == "[]":
			return true
		case strings.HasPrefix(l, `{"`) || strings.HasPrefix(l, "[{") || strings.HasPrefix(l, `["`):
			return true
		}
	}
	return false
}

// relativeNote is the one line saying what the shortened names are relative to.
func relativeNote(modules []string) string {
	seen := map[string]bool{}
	var uniq []string
	for _, m := range modules {
		if m != "" && !seen[m] {
			seen[m] = true
			uniq = append(uniq, m)
		}
	}
	sort.Strings(uniq)
	return "\n\nGo names are shown relative to their module (" + strings.Join(uniq, ", ") + "); pass either form as input."
}

// outputModeOf reads a call's output_mode argument, "" when it has none.
func outputModeOf(raw json.RawMessage) string {
	var args struct {
		OutputMode string `json:"output_mode"`
	}
	_ = json.Unmarshal(raw, &args)
	return strings.TrimSpace(args.OutputMode)
}
