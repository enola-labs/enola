package javaextractor

import (
	"strings"

	"github.com/enola-labs/enola/internal/facts"
)

// A simple type name a file neither declares nor imports by name is looked up as
// javac does: in the file's own package, then in each package it imports on
// demand (`import com.acme.mapper.*;`). The walker sees one file and cannot tell
// which declares the name, so it writes the choice, `{own.Name|wc1.Name|...}`,
// wherever it writes a type: a supertype, a `new`, an injected type, a field's,
// local's or parameter's type, a method's return type. A call on such a receiver
// may append to it (`{...}.make()`, factoryreturns.go).
//
// resolveTypeChoices replaces each choice, once every declared type is indexed,
// by the first candidate the repository declares, or by the own-package reading
// when none is: a library type imported on demand is not guessed at.

// typeChoice returns the own-package FQN alone when the file imports nothing on
// demand, and the choice otherwise.
func typeChoice(own string, wildcards []string, simple string) string {
	if len(wildcards) == 0 {
		return own
	}
	var b strings.Builder
	b.WriteString("{")
	b.WriteString(own)
	for _, wc := range wildcards {
		b.WriteString("|")
		b.WriteString(wc)
		b.WriteString(".")
		b.WriteString(simple)
	}
	b.WriteString("}")
	return b.String()
}

// pickType resolves a choice at the head of t against typeIndex and returns t
// with it replaced. t is returned as is when it starts with no choice.
func pickType(t string, typeIndex map[string]string) string {
	if !strings.HasPrefix(t, "{") {
		return t
	}
	end := strings.IndexByte(t, '}')
	if end < 0 {
		return t
	}
	cands := strings.Split(t[1:end], "|")
	chosen := cands[0]
	for _, c := range cands {
		if _, ok := typeIndex[c]; ok {
			chosen = c
			break
		}
	}
	return chosen + t[end+1:]
}

// resolveTypeChoices resolves every choice the walker wrote: in the supertype,
// instantiation and injection targets, the field types, the return types and
// the typed-call notes.
func resolveTypeChoices(all []facts.Fact, typeIndex map[string]string) {
	for i := range all {
		f := &all[i]
		for j := range f.Relations {
			r := &f.Relations[j]
			switch r.Kind {
			case facts.RelImplements, facts.RelInstantiates, facts.RelInjects:
				r.Target = pickType(r.Target, typeIndex)
			}
		}
		if f.Kind != facts.KindSymbol {
			continue
		}
		if ft, ok := f.PropAny(facts.PropFieldTypes).(map[string]string); ok {
			for k, v := range ft {
				ft[k] = pickType(v, typeIndex)
			}
		}
		if rt, ok := f.PropAny(propReturnType).(string); ok {
			f.SetProp(propReturnType, pickType(rt, typeIndex))
		}
		if calls, ok := f.PropAny(propTypedCalls).([]typedCall); ok {
			for k := range calls {
				calls[k].typ = pickType(calls[k].typ, typeIndex)
			}
		}
	}
}
