package rustextractor

import (
	"slices"
	"strings"

	sitter "github.com/enola-labs/enola/internal/treesitter"
)

// A std::io Read or Write method is I/O only when its receiver is a real sink.
//
//	fn write_crate<W: Write>(krate: &Crate, mut writer: W) {
//	    writer.write_all(b"\n")?;        // whatever the caller hands in
//	}
//	let mut body = Vec::new();
//	body.write_all(b"x")?;               // memory
//
// The method name alone says nothing about the receiver. Where the source shows
// the receiver is an in-memory buffer (Vec, String, Cursor, VecDeque, a byte
// slice) or a generic parameter (`W: Write`, `impl Write`), the call is not the
// function's own I/O: a caller that hands in a file or a socket opened it, and
// carries the I/O. Any other receiver, including `dyn Write`, keeps seeding.

// rustIOStreamMethods are the std::io Read and Write methods in rustIOMethods.
// The database and request methods (execute, send, ...) are I/O on a generic
// receiver too, so only these are judged by their receiver.
var rustIOStreamMethods = map[string]bool{
	"write_all": true, "read_to_string": true, "read_to_end": true,
}

// rustBufferTypes are std types whose Read or Write impls touch only memory.
var rustBufferTypes = map[string]bool{
	"Vec": true, "String": true, "Cursor": true, "VecDeque": true,
}

// rustBufferConstructors are the associated functions that build an empty or
// wrapping buffer of a rustBufferTypes type.
var rustBufferConstructors = map[string]bool{
	"new": true, "with_capacity": true, "default": true,
}

// bindBufferParameters records the parameters whose declared type is an
// in-memory buffer or a generic parameter.
func (w *astWalker) bindBufferParameters(params *sitter.Node) {
	if params == nil {
		return
	}
	for i := uint(0); i < params.NamedChildCount(); i++ {
		p := params.NamedChild(i)
		if kindOf(p) != "parameter" {
			continue
		}
		pat := p.ChildByFieldName("pattern")
		if pat == nil || kindOf(pat) != "identifier" {
			continue
		}
		if w.isBufferType(p.ChildByFieldName("type")) {
			w.bufferLocals[nodeText(pat, w.src)] = true
		}
	}
}

// bindBufferLet records or forgets the name a let binds: a buffer when its written
// type or its value shows one, otherwise not (it shadows an earlier buffer).
func (w *astWalker) bindBufferLet(let *sitter.Node) {
	pat := let.ChildByFieldName("pattern")
	if pat == nil || kindOf(pat) != "identifier" {
		return
	}
	name := nodeText(pat, w.src)
	buffer := false
	if typ := let.ChildByFieldName("type"); typ != nil {
		buffer = w.isBufferType(typ)
	} else {
		buffer = w.isBufferValue(let.ChildByFieldName("value"))
	}
	if buffer {
		w.bufferLocals[name] = true
	} else {
		delete(w.bufferLocals, name)
	}
}

// isBufferType reports whether a written type is an in-memory buffer or a
// generic parameter, seen through references.
func (w *astWalker) isBufferType(t *sitter.Node) bool {
	if t == nil {
		return false
	}
	switch kindOf(t) {
	case "reference_type":
		return w.isBufferType(t.ChildByFieldName("type"))
	case "abstract_type": // impl Write
		return true
	case "array_type": // [u8]
		return true
	case "type_identifier":
		name := nodeText(t, w.src)
		return rustBufferTypes[name] || slices.Contains(w.typeParams, name)
	case "generic_type", "scoped_type_identifier":
		return rustBufferTypes[simpleTypeName(t, w.src)]
	}
	return false
}

// isBufferValue reports whether a let's value builds an in-memory buffer:
// `Vec::new()`, `Vec::<u8>::with_capacity(n)`, `io::Cursor::new(..)`, `vec![..]`.
func (w *astWalker) isBufferValue(v *sitter.Node) bool {
	if v == nil {
		return false
	}
	switch kindOf(v) {
	case "macro_invocation":
		m := v.ChildByFieldName("macro")
		return m != nil && nodeText(m, w.src) == "vec"
	case "call_expression":
		fn := v.ChildByFieldName("function")
		if fn == nil || kindOf(fn) != "scoped_identifier" {
			return false
		}
		name, path := fn.ChildByFieldName("name"), fn.ChildByFieldName("path")
		if name == nil || path == nil || !rustBufferConstructors[nodeText(name, w.src)] {
			return false
		}
		leaf := nodeText(path, w.src)
		if i := strings.Index(leaf, "<"); i >= 0 {
			leaf = strings.TrimSuffix(strings.TrimSpace(leaf[:i]), "::")
		}
		if i := strings.LastIndex(leaf, "::"); i >= 0 {
			leaf = leaf[i+2:]
		}
		return rustBufferTypes[leaf]
	}
	return false
}

// ioOnBuffer reports whether a callee is a std::io stream method called on a
// local the source shows is an in-memory buffer or a generic parameter.
func (w *astWalker) ioOnBuffer(fn *sitter.Node) bool {
	if fn != nil && kindOf(fn) == "generic_function" {
		fn = fn.ChildByFieldName("function")
	}
	if fn == nil || kindOf(fn) != "field_expression" {
		return false
	}
	field := fn.ChildByFieldName("field")
	if field == nil || !rustIOStreamMethods[nodeText(field, w.src)] {
		return false
	}
	recv := fn.ChildByFieldName("value")
	for recv != nil {
		switch kindOf(recv) {
		case "identifier":
			return w.bufferLocals[nodeText(recv, w.src)]
		case "parenthesized_expression", "reference_expression":
			inner := recv.ChildByFieldName("value")
			if inner == nil && recv.NamedChildCount() > 0 {
				inner = recv.NamedChild(0)
			}
			recv = inner
		default:
			return false
		}
	}
	return false
}
