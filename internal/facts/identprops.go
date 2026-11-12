package facts

import (
	"encoding/json"
	"iter"
	"math"
)

// identProps holds the four props that say WHICH declaration a symbol is and which
// version of it: PropBodyHash, PropInnerHash, PropDeclHash and PropSpanStart. They are
// props on the wire and to every reader, and live here instead of in the Props map.
//
// They are unique per symbol by construction, and a map holding one is unique with it.
// Freeze shares identical Props maps, and on a kernel-sized graph nine in ten facts used
// to share one; once every symbol carried its own hashes, 465k of runtime's 565k maps
// were distinct and the store held about 700 MiB more on linux than the same facts did
// before. Kept out of the map, the rest of a symbol's props share again.
//
// A Fact holds a pointer, nil for every fact without one of the four. The value behind
// it is never written: SetProp and DelProp replace the pointer, because a Fact copied by
// value shares it, as it shares the Props map.
type identProps struct {
	bodyHash, innerHash, declHash string
	spanStart                     int
}

// identKeys are the props identProps holds, in the order encoding/json sorts map keys.
var identKeys = [...]string{PropBodyHash, PropDeclHash, PropInnerHash, PropSpanStart}

func isIdentKey(key string) bool {
	switch key {
	case PropBodyHash, PropInnerHash, PropDeclHash, PropSpanStart:
		return true
	}
	return false
}

// identValue converts v to what the field for key holds, reporting false for a value it
// cannot hold exactly: such a value stays in Props, where it always was.
func identValue(key string, v any) (string, int, bool) {
	if key != PropSpanStart {
		s, ok := v.(string)
		return s, 0, ok && s != ""
	}
	switch n := v.(type) {
	case int:
		return "", n, n > 0
	case float64: // as JSON gives it back
		if n > 0 && n <= math.MaxInt32 && n == math.Trunc(n) {
			return "", int(n), true
		}
	}
	return "", 0, false
}

func (p *identProps) get(key string) (any, bool) {
	if p == nil {
		return nil, false
	}
	switch key {
	case PropBodyHash:
		return p.bodyHash, p.bodyHash != ""
	case PropInnerHash:
		return p.innerHash, p.innerHash != ""
	case PropDeclHash:
		return p.declHash, p.declHash != ""
	case PropSpanStart:
		return p.spanStart, p.spanStart != 0
	}
	return nil, false
}

// str is get for the three string props, without boxing the value into an any: PropString
// is called per fact in passes over the whole store.
func (p *identProps) str(key string) (string, bool) {
	if p == nil {
		return "", false
	}
	switch key {
	case PropBodyHash:
		return p.bodyHash, p.bodyHash != ""
	case PropInnerHash:
		return p.innerHash, p.innerHash != ""
	case PropDeclHash:
		return p.declHash, p.declHash != ""
	}
	return "", false
}

func (p *identProps) count() int {
	if p == nil {
		return 0
	}
	n := 0
	for _, k := range identKeys {
		if _, ok := p.get(k); ok {
			n++
		}
	}
	return n
}

// with returns a copy holding s or n for key; without returns a copy lacking key, or nil
// when nothing is left.
func (p *identProps) with(key, s string, n int) *identProps {
	var c identProps
	if p != nil {
		c = *p
	}
	switch key {
	case PropBodyHash:
		c.bodyHash = s
	case PropInnerHash:
		c.innerHash = s
	case PropDeclHash:
		c.declHash = s
	case PropSpanStart:
		c.spanStart = n
	}
	return &c
}

func (p *identProps) without(key string) *identProps {
	if _, ok := p.get(key); !ok {
		return p
	}
	c := p.with(key, "", 0)
	if c.count() == 0 {
		return nil
	}
	return c
}

// setIdentProp is SetProp for an identity prop, reporting whether it stored val. A value
// the field cannot hold is left for the map, and the field must not shadow it then.
func (f *Fact) setIdentProp(key string, val any) bool {
	if !isIdentKey(key) {
		return false
	}
	if s, n, ok := identValue(key, val); ok {
		f.ident = f.ident.with(key, s, n)
		if f.Props != nil {
			delete(f.Props, key)
		}
		return true
	}
	f.ident = f.ident.without(key)
	return false
}

// liftIdentProps moves the identity props a decoded fact carries in Props onto the fact,
// deleting them from the map, which must be the fact's own and not yet shared.
func (f *Fact) liftIdentProps() {
	if len(f.Props) == 0 {
		return
	}
	// Collected on the stack and allocated once: with() per key would allocate a copy for
	// each of the four, on every symbol a restore reads.
	var p identProps
	if f.ident != nil {
		p = *f.ident
	}
	lifted := false
	for _, k := range identKeys {
		v, ok := f.Props[k]
		if !ok {
			continue
		}
		s, n, ok := identValue(k, v)
		if !ok {
			continue
		}
		switch k {
		case PropBodyHash:
			p.bodyHash = s
		case PropInnerHash:
			p.innerHash = s
		case PropDeclHash:
			p.declHash = s
		case PropSpanStart:
			p.spanStart = n
		}
		delete(f.Props, k)
		lifted = true
	}
	if lifted {
		f.ident = &p
	}
	if len(f.Props) == 0 {
		f.Props = nil
	}
}

// AllProps returns every prop the fact carries, the identity props included, as one map.
// It is Props itself when the fact has none of those, and a new map when it does, so the
// result must be read and not written. Read one prop through PropString and the others;
// this is for a reader that needs the whole set, as a writer of JSON does.
func (f Fact) AllProps() map[string]any {
	if f.ident == nil {
		return f.Props
	}
	all := make(map[string]any, len(f.Props)+4)
	for k, v := range f.Props {
		all[k] = v
	}
	for _, k := range identKeys {
		if v, ok := f.ident.get(k); ok {
			all[k] = v
		}
	}
	return all
}

// PropNames yields the name of every prop the fact carries, the identity props included,
// without building a map. The order is unspecified.
func (f Fact) PropNames() iter.Seq[string] {
	return func(yield func(string) bool) {
		for k := range f.Props {
			if !yield(k) {
				return
			}
		}
		for _, k := range identKeys {
			if _, ok := f.ident.get(k); ok && !yield(k) {
				return
			}
		}
	}
}

// FactJSON is Fact without its methods, so it encodes field by field, with the identity
// props back in Props. Embed it, never Fact, in a struct that is encoded as JSON and adds
// fields beside the fact's: Fact's MarshalJSON would be promoted to the outer struct and
// encode the fact alone, dropping every field added beside it. A test fails on any
// struct in this module that embeds Fact.
type FactJSON Fact

// JSONView returns f as FactJSON, for embedding: see FactJSON.
func (f Fact) JSONView() FactJSON {
	p := FactJSON(f)
	p.Props = f.AllProps()
	return p
}

// jsonViewInto is JSONView building the merged props in scratch, which it clears first,
// for a writer that encodes each view before taking the next: facts.jsonl merges one map
// per symbol, and a fresh one each time was most of what the field added to a run.
func (f Fact) jsonViewInto(scratch map[string]any) FactJSON {
	p := FactJSON(f)
	if f.ident == nil {
		return p
	}
	clear(scratch)
	for k, v := range f.Props {
		scratch[k] = v
	}
	for _, k := range identKeys {
		if v, ok := f.ident.get(k); ok {
			scratch[k] = v
		}
	}
	p.Props = scratch
	return p
}

// MarshalJSON writes the fact with the identity props inside "props", where they have
// always been: the bytes are those of a Fact holding all of its props in the map.
func (f Fact) MarshalJSON() ([]byte, error) {
	return json.Marshal(f.JSONView())
}
