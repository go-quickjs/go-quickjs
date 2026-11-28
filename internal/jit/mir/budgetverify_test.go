//go:build quickjs_verify

package mir

import (
	"math/rand/v2"
	"testing"
)

// Of random graphs, with random back edges and blocks nothing enters, the
// budget loops findBudgets finds are the reference walks' (checkBudgets,
// which the verifying build runs on every compile, panics where they are
// not): loops entered other than at their headers, by blocks before them,
// in them and after them, and loops each holding the other's header.
func TestBudgetsMatchReference(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for i := range 20000 {
		n := 2 + r.IntN(14)
		var edges [][2]int
		back := map[[2]int]bool{}
		for range r.IntN(3 * n) {
			e := [2]int{r.IntN(n), 1 + r.IntN(n-1)}
			edges = append(edges, e)
			if r.IntN(3) == 0 {
				back[e] = true
			}
		}
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("graph %d, %d blocks, edges %v, back edges %v: %v", i, n, edges, back, p)
				}
			}()
			c := &core{f: budgetGraph(n, edges, back)}
			c.findBudgets()
		}()
	}
}
