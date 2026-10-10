package rubyextractor

import (
	"os"
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"
	ruby "github.com/tree-sitter/tree-sitter-ruby/bindings/go"
)

// Shared Ruby-source helpers: the macro tables the association and Turbo
// readers dispatch on, the two inflections ActiveRecord derives class and file
// names with, and one walk over a file's method calls.

// camelizeClass turns an association name into the class ActiveRecord derives
// from it: from_stage -> FromStage.
func camelizeClass(name string) string {
	var b strings.Builder
	upper := true
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c == '_' {
			upper = true
			continue
		}
		if upper && c >= 'a' && c <= 'z' {
			c = c - 'a' + 'A'
		}
		upper = false
		b.WriteByte(c)
	}
	return b.String()
}

// underscoreClass turns a Ruby class name into its file path: User -> user,
// CustomField::PickedCustomField -> custom_field/picked_custom_field.
func underscoreClass(name string) string {
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c == ':':
			if i+1 < len(name) && name[i+1] == ':' {
				b.WriteByte('/')
				i++
			}
		case c >= 'A' && c <= 'Z':
			if b.Len() > 0 && b.String()[b.Len()-1] != '/' {
				b.WriteByte('_')
			}
			b.WriteByte(c - 'A' + 'a')
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// relationshipMacros are the resource-class macros that declare a relationship,
// mapped to whether the relationship is to-many.
var relationshipMacros = map[string]bool{"has_one": false, "has_many": true}

// associationMacros are the ActiveRecord macros whose class_name: answers what a
// relationship points at.
var associationMacros = map[string]bool{
	"belongs_to": true, "has_one": true, "has_many": true,
	"has_and_belongs_to_many": true,
}

// eachCall visits every call in a Ruby file, at any nesting depth, so a macro
// inside `module X; class Y` is seen without tracking the enclosing scopes.
func eachCall(path string, visit func(method string, args *sitter.Node, src []byte)) {
	eachCallWithOptions(path, func(method string, args *sitter.Node, _ []*sitter.Node, src []byte) {
		visit(method, args, src)
	})
}

// eachCallWithOptions is eachCall for a reader that honours `with_options`:
//
//	with_options class_name: 'Block', dependent: :destroy do
//	  has_many :block_relationships, foreign_key: 'account_id'
//	  has_many :blocked_by_relationships, foreign_key: :target_account_id
//	end
//
// Every call in the block is made with those options merged in, so the two
// associations above are Blocks. shared holds the argument lists of the enclosing
// with_options calls, innermost first, for the visitor to fall back on.
func eachCallWithOptions(path string, visit func(method string, args *sitter.Node, shared []*sitter.Node, src []byte)) {
	src, err := os.ReadFile(path)
	if err != nil {
		return
	}
	parser := sitter.NewParser()
	defer parser.Close()
	if err := parser.SetLanguage(sitter.NewLanguage(ruby.Language())); err != nil {
		return
	}
	tree := parser.Parse(src, nil)
	defer tree.Close()

	var walk func(n *sitter.Node, shared []*sitter.Node)
	walk = func(n *sitter.Node, shared []*sitter.Node) {
		for i := uint(0); i < n.ChildCount(); i++ {
			c := n.Child(i)
			inner := shared
			if kindOf(c) == "call" || kindOf(c) == "method_call" {
				method, args := rubyText(c.ChildByFieldName("method"), src), c.ChildByFieldName("arguments")
				visit(method, args, shared, src)
				if method == "with_options" && args != nil && c.ChildByFieldName("block") != nil {
					inner = append([]*sitter.Node{args}, shared...)
				}
			}
			if kindOf(c) == "identifier" && c.Parent() != nil && kindOf(c.Parent()) == "body_statement" {
				visit(rubyText(c, src), nil, shared, src)
			}
			walk(c, inner)
		}
	}
	walk(tree.RootNode(), nil)
}
