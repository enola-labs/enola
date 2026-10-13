package tsextractor

import (
	"bytes"
	"regexp"

	"github.com/enola-labs/enola/internal/litfold"
)

var (
	fetchNames  = [][]byte{[]byte("fetch"), []byte("makeRequest")}
	urlWord     = []byte("url")
	exportsWord = []byte("exports")
	useMember   = []byte("use")
)

func findAllSubmatchIf(ok bool, re *regexp.Regexp, src []byte) [][][]byte {
	if !ok {
		return nil
	}
	return re.FindAllSubmatch(src, -1)
}

func findAllSubmatchIndexIf(ok bool, re *regexp.Regexp, src []byte) [][]int {
	if !ok {
		return nil
	}
	return re.FindAllSubmatchIndex(src, -1)
}

func hasMemberAccess(src, member []byte) bool {
	for off := 0; ; {
		i := bytes.Index(src[off:], member)
		if i < 0 {
			return false
		}
		j := off + i - 1
		for j >= 0 && isRegexpSpace(src[j]) {
			j--
		}
		if j >= 0 && src[j] == '.' {
			return true
		}
		off += i + 1
	}
}

func isRegexpSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\f' || c == '\r'
}

func routerVerbCalls(src []byte, routers map[string]bool) [][]int {
	if len(routers) == 0 {
		return nil
	}
	return serverVerbCall.FindAllSubmatchIndex(src, -1)
}

func fileAssignments(src []byte) *litfold.Assignments {
	folds := litfold.NewAssignments()
	declared := map[int]bool{}
	for _, m := range urlAssignDecl.FindAllSubmatchIndex(src, -1) {
		folds.Add(string(src[m[2]:m[3]]), firstNonEmptyGroup(src, m, 2, 3, 4))
		declared[m[2]] = true
	}
	for _, m := range bareAssign.FindAllSubmatchIndex(src, -1) {
		name := string(src[m[2]:m[3]])
		if declared[m[2]] || name == "const" || name == "let" || name == "var" ||
			name == "return" || name == "typeof" || name == "await" {
			continue
		}
		folds.Add(name, "")
	}
	return folds
}

var kafkaWord = []byte("kafka")

var (
	requireWord = []byte("require")
	importWord  = []byte("import")
)

func mayCallRequireOrImport(src []byte) bool {
	if bytes.Contains(src, requireWord) {
		return true
	}
	for off := 0; ; {
		i := bytes.Index(src[off:], importWord)
		if i < 0 {
			return false
		}
		j := off + i + len(importWord)
		for j < len(src) && isRegexpSpace(src[j]) {
			j++
		}
		if j < len(src) && (src[j] == '(' || src[j] == '/' || src[j] == '.') {
			return true
		}
		if bytes.HasPrefix(src[j:], deferWord) || bytes.HasPrefix(src[j:], sourceWord) {
			return true
		}
		off += i + 1
	}
}

var (
	deferWord  = []byte("defer")
	sourceWord = []byte("source")
)

var constWord = []byte("const")

func mayBindLiteral(text []byte) bool {
	if !bytes.Contains(text, constWord) {
		return false
	}
	for i := 0; i < len(text); i++ {
		if text[i] != '=' {
			continue
		}
		j := i + 1
		for j < len(text) && isRegexpSpace(text[j]) {
			j++
		}
		if j < len(text) && (text[j] == '[' || text[j] == '{' || text[j] == '/') {
			return true
		}
	}
	return false
}
