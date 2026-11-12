package tree_sitter

import (
	"os"
	"slices"
	"sync"
	"testing"
	"unsafe"

	csharp "github.com/enola-labs/enola/internal/extractors/dotnetextractor/grammar"
)

// A slab must fill its size class: one byte over 8 KiB and Go rounds it up to 9,472.
func TestNodeSlab_FillsItsSizeClass(t *testing.T) {
	if size := unsafe.Sizeof(nodeSlab{}); size > 8192 || size < 8192-unsafe.Sizeof(Node{}) {
		t.Fatalf("nodeSlab is %d bytes; size nodeSlabSize so it fills 8192 without crossing it", size)
	}
}

// walkKinds lists every node's kind in pre-order, through Child, the allocating accessor.
func walkKinds(n *Node, out []string) []string {
	out = append(out, n.Kind())
	for i := uint(0); i < n.ChildCount(); i++ {
		out = walkKinds(n.Child(i), out)
	}
	return out
}

// Nodes carved from pooled slabs by many goroutines at once are each their own: every
// goroutine walks its own tree and sees exactly what one goroutine alone sees. Run with
// -race, this is the check that no two goroutines ever carve from one slab.
func TestNodeSlab_ConcurrentWalksSeeTheirOwnNodes(t *testing.T) {
	src, err := os.ReadFile("testdata/recovery.cs")
	if err != nil {
		t.Fatal(err)
	}
	parse := func() *Tree {
		p := NewParser()
		defer p.Close()
		if err := p.SetLanguage(NewLanguage(csharp.Language())); err != nil {
			t.Error(err)
			return nil
		}
		return p.Parse(src, nil)
	}
	want := func() []string {
		tree := parse()
		defer tree.Close()
		return walkKinds(tree.RootNode(), nil)
	}()
	if len(want) < nodeSlabSize/4 {
		t.Fatalf("fixture has %d nodes; too few to share slabs between walks", len(want))
	}

	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tree := parse()
			if tree == nil {
				return
			}
			defer tree.Close()
			for round := 0; round < 20; round++ {
				if got := walkKinds(tree.RootNode(), nil); !slices.Equal(got, want) {
					t.Errorf("a concurrent walk saw %d nodes differing from a lone walk's %d", len(got), len(want))
					return
				}
			}
		}()
	}
	wg.Wait()
}
