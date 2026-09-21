// Package hclextractor extracts architectural facts from Terraform/HCL.
package hclextractor

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/enola-labs/enola/internal/extractors/detectnames"
	"github.com/enola-labs/enola/internal/factpath"
	"github.com/enola-labs/enola/internal/facts"
)

// HCL is a data language whose block grammar is line-regular enough for exact
// scanning — the gRPC/OpenAPI hand-rolled tradition, no grammar dependency.
// A Terraform directory is a module; blocks declare addresses (aws_instance.web,
// module.vpc, var.region) and reference each other by exactly those addresses,
// so every edge below is a literal the source states:
//
//   - var./local./module./data. prefixed references,
//   - bare resource addresses, matched ONLY against the address set declared in
//     the same directory (Terraform's own scoping), so prose or function names
//     can never fabricate an edge,
//   - explicit depends_on lists,
//   - a module block's literal local source, which also draws the
//     directory-level dependency.
//
// Remote module sources (registry, git) are counted as external references and
// draw nothing — a missing edge beats a wrong one.

// Extractor extracts Terraform/HCL facts.
type Extractor struct{}

// New creates the extractor.
func New() *Extractor { return &Extractor{} }

// Name returns the extractor name.
func (e *Extractor) Name() string { return "hcl" }

// OwnsFile scopes caching and test-ref handoff to HCL sources.
func (e *Extractor) OwnsFile(relFile string) bool { return isHCLFile(relFile) }

func isHCLFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".tf" || ext == ".hcl"
}

// Detect probes for any .tf/.hcl file within three directory levels.
func (e *Extractor) Detect(repoPath string) (bool, error) {
	return e.DetectFiles(repoPath, detectnames.Walk(repoPath))
}

// DetectFiles implements plugin.FileListDetector: a .tf/.hcl file at any depth,
// replacing a three-level scan that missed modules/*/*/*/main.tf in any layered
// Terraform layout.
func (e *Extractor) DetectFiles(_ string, files []string) (bool, error) {
	for _, rel := range files {
		if detectnames.HasSegment(rel, ".terraform") {
			continue
		}
		if isHCLFile(rel) {
			return true, nil
		}
	}
	return false, nil
}

var (
	labeledBlock  = regexp.MustCompile(`(?m)^\s*(resource|data)\s+"([^"]+)"\s+"([^"]+)"\s*\{`)
	namedBlock    = regexp.MustCompile(`(?m)^\s*(module|variable|output|provider)\s+"([^"]+)"\s*\{`)
	localsAssign  = regexp.MustCompile(`(?m)^\s{2}([A-Za-z_][\w-]*)\s*=`)
	prefixedRef   = regexp.MustCompile(`\b(var|local|module|data)\.([A-Za-z_][\w-]*)(?:\.([A-Za-z_][\w-]*))?`)
	moduleSource  = regexp.MustCompile(`(?m)^\s*source\s*=\s*"([^"]+)"`)
	dependsOnList = regexp.MustCompile(`(?ms)depends_on\s*=\s*\[([^\]]*)\]`)
	bareAddress   = regexp.MustCompile(`\b([a-z][a-z0-9_]*)\.([A-Za-z_][\w-]*)\b`)
)

type hclBlock struct {
	kind    string // resource | data | module | variable | output | provider | local
	address string // aws_instance.web | module.vpc | var.region | output.url | local.tags
	file    string
	line    int
	body    string
}

// Extract parses HCL files and emits facts. Directories are processed as
// Terraform modules: declared addresses are collected per directory first, so
// bare-address references resolve only against what that module actually
// declares.
func (e *Extractor) Extract(ctx context.Context, repoPath string, files []string) ([]facts.Fact, error) {
	byDir := map[string][]string{}
	for _, relFile := range files {
		if isHCLFile(relFile) {
			dir := factpath.Dir(relFile)
			byDir[dir] = append(byDir[dir], relFile)
		}
	}
	dirs := make([]string, 0, len(byDir))
	for d := range byDir {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)

	var out []facts.Fact
	for _, dir := range dirs {
		sort.Strings(byDir[dir])
		var blocks []hclBlock
		for _, relFile := range byDir[dir] {
			src, err := os.ReadFile(filepath.Join(repoPath, relFile))
			if err != nil {
				continue
			}
			blocks = append(blocks, scanHCLBlocks(string(src), relFile)...)
		}
		if len(blocks) == 0 {
			continue
		}
		declared := map[string]bool{}
		for _, b := range blocks {
			declared[b.address] = true
		}
		prefix := dir + "."
		if dir == "." {
			prefix = ""
		}
		out = append(out, facts.Fact{
			Kind:  facts.KindModule,
			Name:  dir,
			File:  dir,
			Props: map[string]any{"language": "hcl"},
		})
		for _, b := range blocks {
			f := facts.Fact{
				Kind: facts.KindSymbol,
				Name: prefix + b.address,
				File: b.file,
				Line: b.line,
				Props: map[string]any{
					"language":    "hcl",
					"symbol_kind": "type",
					"hcl_block":   b.kind,
					"exported":    b.kind == "output" || b.kind == "variable",
				},
				Relations: []facts.Relation{{Kind: facts.RelDeclares, Target: dir}},
			}
			for _, target := range hclReferences(b, declared) {
				if target == b.address {
					continue
				}
				f.Relations = append(f.Relations, facts.Relation{Kind: facts.RelDependsOn, Target: prefix + target})
			}
			if b.kind == "module" {
				if sm := moduleSource.FindStringSubmatch(b.body); sm != nil {
					src := sm[1]
					if strings.HasPrefix(src, "./") || strings.HasPrefix(src, "../") {
						resolved := factpath.Clean(factpath.Join(dir, src))
						out = append(out, facts.Fact{
							Kind: facts.KindDependency,
							Name: dir + " -> " + resolved,
							File: b.file,
							Line: b.line,
							Props: map[string]any{
								"language": "hcl",
								"source":   "internal",
							},
							Relations: []facts.Relation{{Kind: facts.RelImports, Target: resolved}},
						})
					} else {
						f.SetProp("module_source", src)
						f.SetProp("external", true)
					}
				}
			}
			out = append(out, f)
		}
	}
	return out, nil
}

// scanHCLBlocks reads a file's top-level blocks with brace-depth extents.
func scanHCLBlocks(text, relFile string) []hclBlock {
	var blocks []hclBlock
	type opening struct {
		kind, address string
		start         int
		line          int
	}
	var opens []opening
	for _, m := range labeledBlock.FindAllStringSubmatchIndex(text, -1) {
		kind := text[m[2]:m[3]]
		addr := text[m[4]:m[5]] + "." + text[m[6]:m[7]]
		if kind == "data" {
			addr = "data." + addr
		}
		opens = append(opens, opening{kind: kind, address: addr, start: m[0], line: 1 + strings.Count(text[:m[0]], "\n")})
	}
	for _, m := range namedBlock.FindAllStringSubmatchIndex(text, -1) {
		kind := text[m[2]:m[3]]
		name := text[m[4]:m[5]]
		addr := name
		switch kind {
		case "module":
			addr = "module." + name
		case "variable":
			addr = "var." + name
		case "output":
			addr = "output." + name
		case "provider":
			addr = "provider." + name
		}
		opens = append(opens, opening{kind: kind, address: addr, start: m[0], line: 1 + strings.Count(text[:m[0]], "\n")})
	}
	searchFrom := 0
	for {
		idx := strings.Index(text[searchFrom:], "locals {")
		if idx < 0 {
			break
		}
		idx += searchFrom
		body := hclBlockBody(text, idx)
		for _, lm := range localsAssign.FindAllStringSubmatchIndex(body, -1) {
			blocks = append(blocks, hclBlock{
				kind:    "local",
				address: "local." + body[lm[2]:lm[3]],
				file:    relFile,
				line:    1 + strings.Count(text[:idx], "\n") + strings.Count(body[:lm[0]], "\n"),
				body:    "",
			})
		}
		searchFrom = idx + len("locals {") + len(body)
	}
	sort.Slice(opens, func(i, j int) bool { return opens[i].start < opens[j].start })
	for _, o := range opens {
		blocks = append(blocks, hclBlock{
			kind:    o.kind,
			address: o.address,
			file:    relFile,
			line:    o.line,
			body:    hclBlockBody(text, o.start),
		})
	}
	return blocks
}

// hclBlockBody returns the brace-delimited body starting at the block opening.
func hclBlockBody(text string, start int) string {
	open := strings.IndexByte(text[start:], '{')
	if open < 0 {
		return ""
	}
	i := start + open + 1
	depth := 1
	for i < len(text) && depth > 0 {
		switch text[i] {
		case '{':
			depth++
		case '}':
			depth--
		case '"':
			i++
			for i < len(text) && text[i] != '"' {
				if text[i] == '\\' {
					i++
				}
				i++
			}
		case '#':
			for i < len(text) && text[i] != '\n' {
				i++
			}
		}
		i++
	}
	return text[start+open+1 : i-1]
}

// hclReferences collects the literal addresses a block's body references,
// sorted and deduplicated. Bare two-segment tokens count only when the same
// directory declares that exact address.
func hclReferences(b hclBlock, declared map[string]bool) []string {
	seen := map[string]bool{}
	for _, m := range prefixedRef.FindAllStringSubmatch(b.body, -1) {
		prefix, name := m[1], m[2]
		switch prefix {
		case "var", "local":
			seen[prefix+"."+name] = true
		case "module":
			seen["module."+name] = true
		case "data":
			if m[3] != "" {
				seen["data."+name+"."+m[3]] = true
			}
		}
	}
	for _, dm := range dependsOnList.FindAllStringSubmatch(b.body, -1) {
		for _, am := range bareAddress.FindAllStringSubmatch(dm[1], -1) {
			addr := am[1] + "." + am[2]
			if declared[addr] {
				seen[addr] = true
			}
		}
	}
	for _, am := range bareAddress.FindAllStringSubmatch(b.body, -1) {
		addr := am[1] + "." + am[2]
		if declared[addr] {
			seen[addr] = true
		}
	}
	refs := make([]string, 0, len(seen))
	for r := range seen {
		refs = append(refs, r)
	}
	sort.Strings(refs)
	return refs
}
