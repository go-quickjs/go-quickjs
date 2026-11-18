//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"testing"
)

// forInStartSetup's loops start over objects whose keys their layout's
// cache has (mir's forInStart) and over every object it must leave to Go:
// one with elements, one whose prototype gained an element or an
// enumerable key after its loops ran, one of no prototype, a class's
// instance, a dictionary, an array, a proxy, a string, null -- and loops
// inside loops, whose outer cursor is not the last done, and a key deleted
// as a loop walks.
const forInStartSetup = `
var o = { a: 1, b: 2, c: 3, d: 4 }
class C { constructor() { this.x = 1; this.y = 2 } m() { return 1 } }
var inst = new C()
var proto = { }
var child = Object.create(proto); child.p = 1; child.q = 2
var bare = Object.create(null); bare.u = 1; bare.v = 2
var withElems = { a: 1 }; withElems[0] = 'e'
var dict = { }; for (var i = 0; i < 40; i++) dict['k' + i] = i; delete dict.k3
var arr = [5, 6, 7]
var prox = new Proxy({ a: 1, b: 2 }, { })
function keys(x) { var s = ''; for (var k in x) s += k; return s }
function count(x, n) { var c = 0; for (var j = 0; j < n; j++) for (var k in x) c += k.length; return c }
function brk(x, n) { var c = 0; for (var j = 0; j < n; j++) { for (var k in x) { c++; break } } return c }
function nested(x) { var s = ''; for (var k in x) { for (var l in x) s += k + l; s += ';' } return s }
function del(x) { var s = ''; for (var k in x) { s += k; delete x.c } x.c = 3; return s }
function all() {
	return [keys(o), keys(inst), keys(child), keys(bare), keys(withElems), keys(dict).length, keys(arr), keys(prox),
		keys('ab'), keys(null), nested(o), del({ a: 1, b: 2, c: 3, d: 4 }), brk(o, 5), count(inst, 5)].join('|')
}
function grow() { proto[0] = 'z'; proto.e = 1; return all() }
`

// forInStartWant and forInStartGrown are all()'s answers before and after
// grow(), Node's (v26.10.0).
const (
	forInStartWant  = "abcd|xy|pq|uv|0a|108|012|ab|01||aaabacad;babbbcbd;cacbcccd;dadbdcdd;|abd|5|10"
	forInStartGrown = "abcd|xy|pq0e|uv|0a|108|012|ab|01||aaabacad;babbbcbd;cacbcccd;dadbdcdd;|abd|5|10"
)

// A for...in's start over an ordinary object whose keys its layout's cache
// has, its prototypes none or a chain the realm found no enumerable key on,
// takes the runtime's last cursor natively, as V8's ForInPrepare takes the
// receiver's map's enum cache (mir's forInStart): a loop over such an
// object makes no call of Go at all, where its start had been one each
// (jitForInPlain), the round trip the most of what for_in_break spent.
// Every answer is Node's and the interpreter's, idle and while the
// collector marks, the cursor's pointer words written through the write
// barrier then: the loops make the same calls either way.
func TestJITForInStart(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
				jitClobberStoresForTest(t)
			}
			var rounds []string
			for range 30 {
				rounds = append(rounds, "all()", "count(o, 200)", "brk(o, 200)", "count(inst, 200)")
			}
			r := jitAgreesForTest(t, forInStartSetup, rounds...)
			v, err := r.Run(compileForTest(t, "all()"))
			if err != nil || v.String().Go() != forInStartWant {
				t.Fatalf("all() = %v, %v; want Node's %s", v, err, forInStartWant)
			}
			for _, c := range []struct {
				src, want string
			}{
				{`''+count(o, 1000)`, "4000"},
				{`''+brk(o, 1000)`, "1000"},
				{`''+count(inst, 1000)`, "2000"},
			} {
				v, calls := jitCountGoCalls(t, r, c.src)
				if v.String().Go() != c.want {
					t.Fatalf("%s = %v, want %s", c.src, v, c.want)
				}
				// A thousand loops, each started and stepped natively: at
				// most a loop's polls are calls of Go.
				if calls > 10 {
					t.Errorf("%s made %d calls of Go; want none a loop", c.src, calls)
				}
			}
			// A prototype that gains an element, which no validity cell
			// sees, or an enumerable key, which invalidates its chain's:
			// the loops over its objects see both.
			v, err = r.Run(compileForTest(t, "grow()"))
			if err != nil || v.String().Go() != forInStartGrown {
				t.Fatalf("grow() = %v, %v; want Node's %s", v, err, forInStartGrown)
			}
		})
	}
}
