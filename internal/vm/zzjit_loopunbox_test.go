//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
)

// loopUnboxSetup's functions each make a loop of a built-in's call with its
// callback inlined: the loop's index and the callback's accumulator are
// numbers on every path, which ssa's unboxPhis keeps unboxed across the
// loop, though only uses that leave a value not a number to Go unbox them.
// Each then meets an exit inside the loop -- a hole, a string among the
// numbers, a shape it was not compiled for -- where the boxes frame states
// name are made of the unboxed numbers, and Go goes on with the loop.
const loopUnboxSetup = `
	var nums = [], mixed = [], holey = []
	for (let i = 0; i < 40; i++) { nums.push(i * 1.5 - 7); mixed.push(i); holey.push(i) }
	mixed[31] = '5'
	delete holey[33]
	var i32 = new Int32Array(40)
	for (let i = 0; i < 40; i++) i32[i] = i * 3 - 50
	var objs = []
	for (let i = 0; i < 12; i++) objs.push({ v: i })
	var odd = { w: 1, v: -9 }
	var sum = 0
	function reduceSum(a) { return a.reduce((s, x) => s + x, 0) }
	function reduceScaled(a, k) { return a.reduce((s, x, i) => s * 0.5 + x * k - i, 1) }
	function mapDouble(a) { return a.map((x, i) => x * 2 + i).join() }
	function someAbove(a, k) { return a.some((x) => x > k) }
	function findAbove(a, k) { return a.findIndex((x) => x > k) }
	function typedSum(k) { return i32.reduce((s, x) => s + x * k, 0) }
	function eachSum(a) { sum = 0; a.forEach((x) => { sum += x }); return sum }
	function shapes(at) { return objs.reduce((s, o, i) => s + (i === at ? odd : o).v, 0) }
`

// loopUnboxNames are loopUnboxSetup's functions whose loops the test counts
// boxes in.
var loopUnboxNames = []string{"reduceSum", "reduceScaled", "mapDouble", "someAbove", "findAbove", "typedSum"}

// The array methods' loops keep their index and their callbacks'
// accumulators unboxed: no box is made inside the loop where only frame
// states name it, and every answer is the interpreter's, whatever leaves
// native code inside the loop, under a budget of polls and finished
// invocations or not.
func TestJITLoopPhisUnboxed(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	listings := map[string]string{}
	mir.Annotate = true
	JITCodeHook = func(f JITFunction, _ []byte, c *mir.Code) { listings[f.Name] = c.Listing }
	defer func() { JITCodeHook, mir.Annotate = nil, false }()
	drivers := ""
	for _, name := range append(loopUnboxNames, "eachSum", "shapes") {
		drivers += "function drive_" + name + "(n, a, k) { let r; for (let i = 0; i < n; i++) r = " + name + "(a, k); return r };"
	}
	var numbers, exits []string
	for i := range 6 {
		a, k, at := "nums", "2", "99"
		if i >= 4 {
			a, k, at = []string{"mixed", "holey"}[i-4], "3", "5"
		}
		round := []string{`drive_reduceSum(30, ` + a + `)`, `drive_reduceScaled(30, ` + a + `, ` + k + `)`, `drive_mapDouble(30, ` + a + `)`,
			`drive_someAbove(30, ` + a + `, 25)`, `drive_someAbove(30, ` + a + `, 99)`, `drive_findAbove(30, ` + a + `, 25)`, `drive_findAbove(30, ` + a + `, 99)`,
			`drive_typedSum(30, ` + k + `)`, `drive_eachSum(30, ` + a + `)`, `drive_shapes(30, ` + at + `)`}
		if i < 4 {
			numbers = append(numbers, round...)
		}
		exits = append(exits, round...)
	}
	setup := loopUnboxSetup + drivers
	// The code compiled while the arrays hold numbers: the callbacks
	// inlined, their loops' numbers unboxed. (Those that then leave at
	// every call are called natively again, by design: jitInlineLeft.)
	jitAgreesForTest(t, setup, numbers...)
	for _, name := range loopUnboxNames {
		l, ok := listings[name]
		if !ok {
			t.Errorf("%s was not compiled", name)
			continue
		}
		if n := innermostLoopBoxes(l); n != 0 {
			t.Errorf("%s makes %d boxes inside its innermost loops:\n%s", name, n, l)
		}
	}
	jitAgreesForTest(t, setup, exits...)
	for _, c := range []jitStressConfig{{threshold: true, budget: 3}, {threshold: true, budget: 3, deopt: 7}} {
		jitAgreesForTestWith(t, c, setup, exits...)
	}
}

// innermostLoopBoxes is how many boxes of numbers (boxf) a value uses inside
// the innermost loops of listing, an ssa.Func's: those that are made at
// every turn, where one only frame states name is made at an exit alone.
func innermostLoopBoxes(listing string) int {
	head := regexp.MustCompile(`^b(\d+) \(pc -?\d+\)(?: <- ([b\d ]+))?`)
	value := regexp.MustCompile(`^\s+(v\d+) = (\S+) \S+(.*)$`)
	ref := regexp.MustCompile(`\bv\d+\b`)
	type block struct {
		preds []int
		vals  [][3]string
	}
	blocks := map[int]*block{}
	var order []int
	var cur *block
	used := map[string]bool{}
	for _, line := range strings.Split(listing, "\n") {
		if m := head.FindStringSubmatch(line); m != nil {
			id, _ := strconv.Atoi(m[1])
			cur = &block{}
			for _, p := range strings.Fields(m[2]) {
				n, _ := strconv.Atoi(strings.TrimPrefix(p, "b"))
				cur.preds = append(cur.preds, n)
			}
			blocks[id] = cur
			order = append(order, id)
			continue
		}
		if cur == nil {
			continue
		}
		if m := value.FindStringSubmatch(line); m != nil {
			cur.vals = append(cur.vals, [3]string{m[1], m[2]})
			for _, r := range ref.FindAllString(strings.Split(m[3], "@")[0], -1) {
				used[r] = true
			}
		} else if s := strings.TrimSpace(line); strings.HasPrefix(s, "if ") || strings.HasPrefix(s, "return ") {
			for _, r := range ref.FindAllString(s, -1) {
				used[r] = true
			}
		}
	}
	pos := map[int]int{}
	for i, id := range order {
		pos[id] = i
	}
	loops := map[int]map[int]bool{}
	for _, h := range order {
		for _, p := range blocks[h].preds {
			if q, ok := pos[p]; !ok || q < pos[h] {
				continue
			}
			in := loops[h]
			if in == nil {
				in = map[int]bool{h: true}
				loops[h] = in
			}
			for work := []int{p}; len(work) != 0; {
				x := work[len(work)-1]
				work = work[:len(work)-1]
				if in[x] {
					continue
				}
				in[x] = true
				work = append(work, blocks[x].preds...)
			}
		}
	}
	n := 0
	for h, in := range loops {
		innermost := true
		for h2 := range loops {
			if h2 != h && in[h2] {
				innermost = false
			}
		}
		if !innermost {
			continue
		}
		for id := range in {
			for _, v := range blocks[id].vals {
				if v[1] == "boxf" && used[v[0]] {
					n++
				}
			}
		}
	}
	return n
}
