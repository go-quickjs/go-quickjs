package vm

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
)

// A random program of empty literals grown past what a site learns, and of
// everything an array's room could show through: holes, a length written,
// elements deleted, a frozen array, a shape of array changed. The reference
// is the program compiled for a debugger, which stays in the interpreter and
// has no room at any literal.
func arrayFuzzSource(seed int) string {
	rng := rand.New(rand.NewPCG(uint64(seed), 0xa22a))
	pick := func(list ...string) string { return list[rng.IntN(len(list))] }
	var b strings.Builder
	b.WriteString(`
function hash(a) {
  let h = a.length | 0, holes = 0
  const step = 1 + (a.length >> 6)
  for (let i = 0; i < a.length; i += step) {
    if (i in a) h = (h * 31 + (typeof a[i] === 'number' ? a[i] : 7)) | 0
    else holes++
  }
  return a.length + ':' + holes + ':' + h + ':' + Object.keys(a).length
}
const log = [], kept = []
`)
	grow := []string{"a.push(i)", "a[a.length] = i", "a[i] = i", "a.push(i, i)", "a.unshift === 0 || a.push(i + 0.5)"}
	after := []string{
		"a.length = k", "a.length = k + 3", "a[k + 2] = 9", "delete a[k]", "a.pop()", "a.shift()", "a.unshift(1)",
		"a.splice(k, 1)", "a.splice(k, 0, 1, 2)", "a.reverse()", "a.fill(3, 0, k)", "a.length = 0", "a = a.slice(k)",
		"a = a.concat([1, 2])", "Object.freeze(a)", "Object.seal(a)", "Object.preventExtensions(a)", "a.foo = 1",
		"a[70000] = 1", "kept.push(a)", "a.sort((x, y) => y - x)", "Object.defineProperty(a, k, { get() { return 1 } })",
		"Object.setPrototypeOf(a, null)", "for (const v of a) { if (v > 5) break }", "a.forEach(v => { if (a.length < 40) a.push(v) })",
	}
	sizes := []string{"0", "3", "40", "255", "256", "257", "300", "300", "1000", "70000", "n", "n", "n >> 1", "n + 9"}
	for f := range 1 + rng.IntN(3) {
		fmt.Fprintf(&b, "function make%d(n) {\n  var a = []\n  for (let i = 0; i < n; i++) %s\n  const k = n >> 1\n", f, pick(grow...))
		for range rng.IntN(3) {
			fmt.Fprintf(&b, "  try { %s } catch (e) { log.push(e.name) }\n", pick(after...))
		}
		b.WriteString("  return a\n}\n")
	}
	fmt.Fprintf(&b, "for (let round = 0; round < %d; round++) {\n  const n = %s\n", 8+rng.IntN(32), pick("300", "300", "400", "1000", "260 + round", "round < 6 ? 300 : 3", "round % 2 ? 300 : 3", "round % 7 ? 300 : 70000"))
	for range 1 + rng.IntN(5) {
		f := rng.IntN(3)
		fmt.Fprintf(&b, "  if (typeof make%d === 'function') { const a = make%d(%s); log.push(hash(a)); if (round === 5) kept.push(a) }\n", f, f, pick(sizes...))
	}
	b.WriteString("}\nlog.concat(kept.map(hash)).join()\n")
	return b.String()
}

func TestArrayRoomRandom(t *testing.T) {
	t.Cleanup(func() { SetTreeTier(true) })
	count := 60
	if testing.Short() {
		count = 6
	}
	first, count := strFuzzRange(t, count)
	for seed := first; seed < first+count; seed++ {
		src := arrayFuzzSource(seed)
		ref := New(Config{Debug: true})
		want := strFuzzRun(t, ref, src, true)
		ref.Close()
		r := New(Config{})
		got := strFuzzRun(t, r, src, false)
		r.Close()
		if got != want {
			t.Fatalf("seed %d: %s\n%s", seed, firstLogDifference(got, want), src)
		}
	}
}
