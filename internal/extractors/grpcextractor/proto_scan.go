package grpcextractor

import (
	"regexp"
	"strings"
)

// protoFile holds the parsed contents of a single .proto source file that are
// architecturally meaningful: the package, the services (each with their RPCs),
// the message names, and the imports.
type protoFile struct {
	pkg      string
	services []protoService
	messages []protoMessage
	imports  []string
}

// protoService is one `service X { ... }` block.
type protoService struct {
	name string
	line int
	rpcs []protoRPC
}

// protoRPC is one `rpc M(Req) returns (Resp);` declaration.
type protoRPC struct {
	name         string
	line         int
	clientStream bool
	serverStream bool
	// input and output are the request and response message types as written.
	input, output string
}

// protoMessage is one `message X { ... }` (or `enum X`) declaration.
type protoMessage struct {
	name string
	line int
	kind string // "message" or "enum"
	// fieldTypes are the message types its fields hold, as written: `Other`,
	// `pkg.Other`, a map's value type. Scalars are left out.
	fieldTypes []string
}

var (
	// The comment/whitespace-tolerant regexes below operate on a
	// comment-stripped, single-token-normalized view of the file (see scanProto).
	rePackage = regexp.MustCompile(`(?m)^\s*package\s+([A-Za-z_][A-Za-z0-9_.]*)\s*;`)
	reImport  = regexp.MustCompile(`(?m)^\s*import\s+(?:public\s+|weak\s+)?"([^"]+)"\s*;`)
	// rpc Name ( [stream] Req ) returns ( [stream] Resp )
	reRPC = regexp.MustCompile(`\brpc\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(\s*(stream\s+)?([.A-Za-z_][.A-Za-z0-9_]*)\s*\)\s+returns\s*\(\s*(stream\s+)?([.A-Za-z_][.A-Za-z0-9_]*)\s*\)`)
	// A field: `[repeated|optional|required] Type name = N`, and a map's value type.
	reField    = regexp.MustCompile(`(?m)(?:^|[;{])\s*(?:repeated\s+|optional\s+|required\s+)?([.A-Za-z_][.A-Za-z0-9_]*)\s+[A-Za-z_][A-Za-z0-9_]*\s*=\s*\d+`)
	reMapField = regexp.MustCompile(`map\s*<\s*[A-Za-z_][A-Za-z0-9_]*\s*,\s*([.A-Za-z_][.A-Za-z0-9_]*)\s*>`)
	reComment  = regexp.MustCompile(`//[^\n]*`)
)

// scanProto parses a .proto source into a protoFile. It is a deliberately small,
// dependency-free proto3 scanner — proto's grammar is regular enough that a
// comment-stripped regex/brace scan captures the service/rpc/message/import
// surface enola needs, mirroring the regex-based OpenAPI and TS-httpclient
// detectors. It is line-accurate: reported lines index the original source.
func scanProto(src []byte) protoFile {
	var pf protoFile

	text := string(src)
	// Strip /* */ block comments (kept line-count-stable is unnecessary; we
	// derive line numbers from the original text via byte offsets below).
	stripped := stripBlockComments(text)
	// Line-comment-free view for the block/regex matching. Keep a parallel
	// original for line lookups.
	noLine := reComment.ReplaceAllString(stripped, "")

	if m := rePackage.FindStringSubmatch(noLine); m != nil {
		pf.pkg = m[1]
	}
	for _, m := range reImport.FindAllStringSubmatch(noLine, -1) {
		pf.imports = append(pf.imports, m[1])
	}

	// Walk service blocks by brace depth so RPCs are attributed to their service.
	pf.services = scanServices(noLine)
	pf.messages = scanMessages(noLine)
	return pf
}

// scanServices finds each `service Name { ... }` block (brace-matched) and the
// RPCs declared directly inside it.
func scanServices(noLine string) []protoService {
	var out []protoService
	reSvc := regexp.MustCompile(`\bservice\s+([A-Za-z_][A-Za-z0-9_]*)\s*\{`)
	for _, loc := range reSvc.FindAllStringSubmatchIndex(noLine, -1) {
		name := noLine[loc[2]:loc[3]]
		openBrace := loc[1] - 1 // index of '{'
		body, _ := matchBrace(noLine, openBrace)
		svc := protoService{
			name: name,
			line: lineAt(noLine, loc[0]),
		}
		for _, rm := range reRPC.FindAllStringSubmatchIndex(body.text, -1) {
			svc.rpcs = append(svc.rpcs, protoRPC{
				name:         body.text[rm[2]:rm[3]],
				line:         lineAt(noLine, body.start+rm[0]),
				clientStream: rm[4] != -1,
				serverStream: rm[8] != -1,
				input:        body.text[rm[6]:rm[7]],
				output:       body.text[rm[10]:rm[11]],
			})
		}
		out = append(out, svc)
	}
	return out
}

// scanMessages finds top-level `message X` and `enum X` names. Nested messages
// are captured too (their names still matter as symbols), keyed by declaration.
func scanMessages(noLine string) []protoMessage {
	var out []protoMessage
	reMsg := regexp.MustCompile(`\b(message|enum)\s+([A-Za-z_][A-Za-z0-9_]*)\s*\{`)
	for _, loc := range reMsg.FindAllStringSubmatchIndex(noLine, -1) {
		msg := protoMessage{
			kind: noLine[loc[2]:loc[3]],
			name: noLine[loc[4]:loc[5]],
			line: lineAt(noLine, loc[0]),
		}
		if msg.kind == "message" {
			body, _ := matchBrace(noLine, loc[1]-1)
			msg.fieldTypes = fieldTypes(body.text)
		}
		out = append(out, msg)
	}
	return out
}

// protoScalars are the field types that name no message.
var protoScalars = map[string]bool{
	"double": true, "float": true, "int32": true, "int64": true, "uint32": true, "uint64": true,
	"sint32": true, "sint64": true, "fixed32": true, "fixed64": true, "sfixed32": true,
	"sfixed64": true, "bool": true, "string": true, "bytes": true,
	// keywords a declaration line can start with, read where a type would be
	"reserved": true, "option": true, "extensions": true,
}

// fieldTypes returns the message types a message body's fields hold, in order,
// once each. A nested message's fields are its own body's and are read with it too:
// the outer message holds the nested one.
func fieldTypes(body string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(t string) {
		if !protoScalars[t] && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	for _, m := range reField.FindAllStringSubmatch(body, -1) {
		add(m[1])
	}
	for _, m := range reMapField.FindAllStringSubmatch(body, -1) {
		add(m[1])
	}
	return out
}

// braceMatch is a substring of the source between a '{' and its matching '}'.
type braceMatch struct {
	text  string
	start int // byte offset of text within the containing string
}

// matchBrace returns the content between the brace at openIdx and its matching
// close brace. If unbalanced, it returns the remainder of the string.
func matchBrace(s string, openIdx int) (braceMatch, bool) {
	depth := 0
	for i := openIdx; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return braceMatch{text: s[openIdx+1 : i], start: openIdx + 1}, true
			}
		}
	}
	return braceMatch{text: s[openIdx+1:], start: openIdx + 1}, false
}

// stripBlockComments blanks /* ... */ spans in place — every byte becomes a
// space except newlines, which are preserved so byte offsets and line numbers
// stay identical to the original source (lineAt relies on this).
func stripBlockComments(s string) string {
	b := []byte(s)
	for i := 0; i < len(b); {
		if i+1 < len(b) && b[i] == '/' && b[i+1] == '*' {
			j := strings.Index(s[i+2:], "*/")
			end := len(b)
			if j >= 0 {
				end = i + 2 + j + 2
			}
			for k := i; k < end; k++ {
				if b[k] != '\n' {
					b[k] = ' '
				}
			}
			i = end
			continue
		}
		i++
	}
	return string(b)
}

// lineAt maps a byte offset in the processed (noLine) view back to a 1-based
// line number in the original source. Both comment-stripping passes preserve
// newline positions (block comments are blanked in place, line comments are
// blanked without touching their trailing newline), so byte offsets — and thus
// a newline count over the processed prefix — line up with the original source.
func lineAt(processed string, off int) int {
	if off > len(processed) {
		off = len(processed)
	}
	if off < 0 {
		off = 0
	}
	return 1 + strings.Count(processed[:off], "\n")
}
