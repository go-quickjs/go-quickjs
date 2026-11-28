//go:build quickjs_verify

package mir

import (
	"fmt"
	"slices"

	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// verifyBudgets is set by the quickjs_verify build tag: findBudgets
// checks its budget loops against budgetsRef's.
const verifyBudgets = true

// checkBudgets panics where findBudgets' loops are not budgetsRef's, or
// their blocks are not.
func (c *core) checkBudgets() {
	steps := c.steps
	headers, of := c.budgetsRef()
	c.steps = steps
	got := make([]*ssa.Block, len(c.budgets))
	for i, b := range c.budgets {
		got[i] = b.header
	}
	if !slices.Equal(got, headers) {
		panic(fmt.Sprintf("mir: budget loops %v, the reference's %v", blockIDs(got), blockIDs(headers)))
	}
	for _, b := range c.f.Blocks {
		if c.loopBudget(b) != ofBudget(of, b) {
			panic(fmt.Sprintf("mir: b%d in budget loop %d, the reference's %d", b.ID, c.loopBudget(b), ofBudget(of, b)))
		}
	}
}

func ofBudget(of []int, b *ssa.Block) int {
	if b.ID < len(of) {
		return of[b.ID] - 1
	}
	return -1
}

func blockIDs(bs []*ssa.Block) []int {
	ids := make([]int, len(bs))
	for i, b := range bs {
		ids[i] = b.ID
	}
	return ids
}

// budgetsRef is the budget loops found as findBudgets first found them, a
// walk of each loop's own -- every block that reaches one of its back
// edges without passing its header, then those of them the header
// reaches -- and by block ID 1 plus the index of the loop each block is
// in. Each loop's walk takes the loops inside it: a nest of d loops' takes
// d²/2 loops' blocks.
func (c *core) budgetsRef() ([]*ssa.Block, []int) {
	f := c.f
	nb := numBlocks(f)
	var headers []*ssa.Block
	var budgetOf []int
	var stack, body []*ssa.Block
	back, fwd := make([]int, nb), make([]int, nb)
	var chains chainTables
	for _, h := range f.Blocks {
		if !h.LoopHeader {
			continue
		}
		mark := h.ID + 1
		back[h.ID] = mark
		stack = stack[:0]
		for i, p := range h.Preds {
			if h.Backedge[i] && back[p.ID] != mark {
				back[p.ID] = mark
				stack = append(stack, p)
			}
		}
		for len(stack) > 0 {
			b := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for _, p := range b.Preds {
				if back[p.ID] != mark {
					back[p.ID] = mark
					stack = append(stack, p)
				}
			}
		}
		fwd[h.ID] = mark
		body = append(body[:0], h)
		stack = append(stack[:0], h)
		for len(stack) > 0 {
			b := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for _, s := range b.Succs {
				if back[s.ID] == mark && fwd[s.ID] != mark {
					fwd[s.ID] = mark
					body = append(body, s)
					stack = append(stack, s)
				}
			}
		}
		ok := true
		for _, b := range body {
			if b != h && b.LoopHeader || b.ID < len(budgetOf) && budgetOf[b.ID] != 0 {
				ok = false
				break
			}
			for _, v := range b.Values {
				if v.Op == ssa.OpCall || v.Op == ssa.OpTailSelf {
					ok = false
					break
				}
			}
			if !ok {
				break
			}
		}
		if !ok || c.turnChain(h, body, fwd, h.ID+1, &chains) >= counterChain {
			continue
		}
		if budgetOf == nil {
			budgetOf = make([]int, nb)
		}
		headers = append(headers, h)
		for _, b := range body {
			budgetOf[b.ID] = len(headers)
		}
	}
	return headers, budgetOf
}
