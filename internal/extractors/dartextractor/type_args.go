package dartextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

type dartSubst struct {
	Owner  *facts.Fact
	Params []string
	Args   []string
	File   string
}

func dartIdentChar(c byte) bool {
	return c == '_' || c == '$' || c == '.' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func dartTypeWords(typ string) []string {
	return strings.FieldsFunc(typ, func(c rune) bool { return c >= 128 || !dartIdentChar(byte(c)) })
}

func dartMentionsAny(typ string, names []string) bool {
	for _, w := range dartTypeWords(typ) {
		for _, n := range names {
			if w == n {
				return true
			}
		}
	}
	return false
}

func dartTypeArguments(typ string) []string {
	typ = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(typ), "?"))
	start := strings.IndexByte(typ, '<')
	if start < 0 || !strings.HasSuffix(typ, ">") {
		return nil
	}
	body := typ[start+1 : len(typ)-1]
	var out []string
	depth, from := 0, 0
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '<', '(', '{', '[':
			depth++
		case '>', ')', '}', ']':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(body[from:i]))
				from = i + 1
			}
		}
	}
	return append(out, strings.TrimSpace(body[from:]))
}

func (r *dartMemberResolver) receiverSubst(typ dartValueType, owner *facts.Fact) *dartSubst {
	if owner == nil || typ.Namespace {
		return nil
	}
	params, _ := owner.Props["type_parameters"].([]string)
	args := dartTypeArguments(typ.Name)
	if len(params) == 0 || len(params) != len(args) {
		return nil
	}
	for _, a := range args {
		if a == "" || a == "dynamic" || strings.Contains(a, "Function") {
			return nil
		}
	}
	return &dartSubst{Owner: owner, Params: params, Args: args, File: typ.File}
}

func (r *dartMemberResolver) substituteType(typ, declFile string, sub *dartSubst) (dartValueType, bool) {
	typ = strings.TrimSpace(typ)
	if typ == "" || typ == "dynamic" || typ == "void" {
		return dartValueType{}, false
	}
	var b strings.Builder
	for i := 0; i < len(typ); {
		if !dartIdentChar(typ[i]) {
			b.WriteByte(typ[i])
			i++
			continue
		}
		j := i
		for j < len(typ) && dartIdentChar(typ[j]) {
			j++
		}
		word := typ[i:j]
		replaced := false
		for k, p := range sub.Params {
			if word == p {
				b.WriteString(sub.Args[k])
				replaced = true
				break
			}
		}
		if !replaced {
			b.WriteString(word)
		}
		i = j
	}
	out := b.String()
	if strings.HasSuffix(out, "??") {
		out = strings.TrimSuffix(out, "?")
	}
	if sub.File != declFile {
		for _, w := range dartTypeWords(out) {
			if r.typeNamed(w, sub.File) != r.typeNamed(w, declFile) {
				return dartValueType{}, false
			}
		}
	}
	return dartValueType{Name: out, File: sub.File}, true
}
