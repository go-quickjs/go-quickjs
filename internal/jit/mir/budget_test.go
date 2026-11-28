package mir

import (
	"slices"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// budgetGraph is a function of n empty blocks and the edges given, the
// marked ones back edges, block 0 its entry.
func budgetGraph(n int, edges [][2]int, back map[[2]int]bool) *ssa.Func {
	f := &ssa.Func{}
	for i := range n {
		f.Blocks = append(f.Blocks, &ssa.Block{ID: i, PC: i})
	}
	f.Blocks[0].PC = -1
	defer func() { f.Entries = []ssa.Entry{{Block: f.Blocks[0]}} }()
	for _, e := range edges {
		from, to := f.Blocks[e[0]], f.Blocks[e[1]]
		from.Succs = append(from.Succs, to)
		to.Preds = append(to.Preds, from)
		to.Backedge = append(to.Backedge, back[e])
		if back[e] {
			to.LoopHeader = true
		}
	}
	return f
}

func budgetHeaders(c *core) []int {
	var ids []int
	for _, b := range c.budgets {
		ids = append(ids, b.header.ID)
	}
	return ids
}

// The budget loops are the loops whose bodies hold no other loop's header:
// of a nest, its innermost; of loops one after another, each; of a loop
// whose body a block outside it enters, one it does not reach, the loop
// still; of a nest of loops entered at their tests, at their ends, none,
// each loop's body holding the other's header. Each is the answer of a
// walk of each loop's own, which the quickjs_verify build checks.
func TestBudgetsInnermost(t *testing.T) {
	for _, tc := range []struct {
		name  string
		n     int
		edges [][2]int
		back  [][2]int
		want  []int
	}{
		// 1 { 2 { 3 } 4 } 5
		{"nest", 6, [][2]int{{0, 1}, {1, 2}, {1, 5}, {2, 3}, {2, 4}, {3, 2}, {4, 1}}, [][2]int{{3, 2}, {4, 1}}, []int{2}},
		// do { do { 2 } while 3; } while 4: the inner loop's way out is its
		// test, then the outer's.
		{"dowhile", 6, [][2]int{{0, 1}, {1, 2}, {2, 3}, {3, 2}, {3, 4}, {4, 1}, {4, 5}}, [][2]int{{3, 2}, {4, 1}}, []int{2}},
		// 1 { 2 } 3 { 4 } 5
		{"sequence", 6, [][2]int{{0, 1}, {1, 2}, {1, 3}, {2, 1}, {3, 4}, {3, 5}, {4, 3}}, [][2]int{{2, 1}, {4, 3}}, []int{1, 3}},
		// 1 { 2 3 } with an entry block 5 going to 6, which goes to 3.
		{"resume", 7, [][2]int{{0, 1}, {1, 2}, {1, 4}, {2, 3}, {3, 1}, {5, 6}, {6, 3}}, [][2]int{{3, 1}}, []int{1}},
		// A loop's back edge from a block only an entry block reaches.
		{"deadback", 5, [][2]int{{0, 1}, {1, 2}, {3, 4}, {4, 1}}, [][2]int{{4, 1}}, []int{1}},
		// 2 { 3 }, which 1, before it, enters at 3, its test at its end.
		{"rotated", 5, [][2]int{{0, 1}, {1, 3}, {2, 3}, {3, 2}, {3, 4}}, [][2]int{{3, 2}}, []int{2}},
		// Two such loops one inside the other, the outer's test 5, the
		// inner's 4: each loop's body, by its header's back edge, holds the
		// other's header, the inner one's by the outer's turn.
		{"rotatednest", 7, [][2]int{{0, 1}, {1, 5}, {5, 2}, {5, 6}, {2, 4}, {4, 3}, {3, 4}, {4, 5}}, [][2]int{{5, 2}, {4, 3}}, nil},
		// A loop whose test, at its end, a block of the loop around it goes
		// to, and the loop's way out goes back to that block.
		{"reentered", 7, [][2]int{{0, 1}, {1, 2}, {2, 3}, {2, 6}, {3, 5}, {4, 5}, {5, 4}, {5, 3}}, [][2]int{{5, 4}, {5, 3}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			back := map[[2]int]bool{}
			for _, e := range tc.back {
				back[e] = true
			}
			c := &core{f: budgetGraph(tc.n, tc.edges, back)}
			c.findBudgets()
			if got := budgetHeaders(c); !slices.Equal(got, tc.want) {
				t.Errorf("budget loops %v, want %v", got, tc.want)
			}
		})
	}
}
