//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"testing"
)

// hasOwnSetup's loop is a React element's props copied and its markup
// written: for (k in props) hop.call(props, k), RESERVED.hasOwnProperty(k),
// pushes of three values, of none and of five, Array.isArray through a
// binding (React's ba) and as a method. others calls the realm's
// Function.prototype.call of what is no leaf: a closure, a built-in that
// is none, a leaf on what it refuses; pushes onto an array whose
// prototype has an index setter, which push must call; and a for...in of
// an object whose prototype is a proxy, whose ownKeys trap must run.
const hasOwnSetup = `
const hop = Object.prototype.hasOwnProperty
const RESERVED = {key: true, ref: true}
const isArr = Array.isArray
function run(n) {
	let t = 0
	const buf = []
	for (let i = 0; i < n; i++) {
		const props = {key: i, a: i, b: 2, ref: 0}, out = {}
		for (const k in props) if (hop.call(props, k) && !RESERVED.hasOwnProperty(k)) out[k] = props[k]
		buf.push("<", "x", ">")
		buf.push()
		t += buf.push(" ", i, "=\"", out.a + out.b, "\"")
		t += isArr(buf) + Array.isArray(out) + isArr(i)
		if (buf.length > 1000) buf.length = 0
	}
	return t + buf.join("").length
}
let sets = 0, traps = 0
const under = Object.create(new Proxy({zz: 1}, {ownKeys(t) { traps++; return Reflect.ownKeys(t) }}))
class Watched extends Array {}
Object.defineProperty(Watched.prototype, 3, {set(v) { sets++ }, configurable: true})
function others(n) {
	let t = 0
	sets = 0, traps = 0
	const add = function (x) { return this.k + x }
	for (let i = 0; i < n; i++) {
		t += add.call({k: i}, 1) + Array.prototype.indexOf.call([1, 2, i], i) + hop.call(new Proxy({a: 1}, {}), "a")
		const w = new Watched()
		w.push(1, 2, 3)
		t += w.push(4, 5) + w.length
		for (const k in under) t += k.length
	}
	return t + "," + sets + "," + traps
}
`

// A call native code makes of hasOwnProperty, as a method or through
// Function.prototype.call, of Array.isArray, or of push of other than one
// value, and a for...in's start, are made
// without a frame (jitPlainCall, jitForInPlain): warm, run leaves its code at none of
// them and makes no call of Go for them. The other calls of call and of
// push are the built-ins'. Each answer is Node's (v26.12.1) and the
// interpreter's.
func TestJITHasOwnAndPushCalls(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	rounds := [][2]string{{"run(300)", "138348"}, {"others(300)", "49647,300,300"}}
	var srcs []string
	for range 6 {
		for _, rd := range rounds {
			srcs = append(srcs, rd[0])
		}
	}
	r := jitAgreesForTest(t, hasOwnSetup, srcs...)
	for _, rd := range rounds {
		v, err := r.Run(compileForTest(t, "''+("+rd[0]+")"))
		if err != nil {
			t.Fatalf("%s: %v", rd[0], err)
		}
		if got := v.String().Go(); got != rd[1] {
			t.Errorf("%s = %s, want Node's %s", rd[0], got, rd[1])
		}
	}
	e := jitNativeForTest(t, r, "run")
	calls, hosts, entries := e.ssaStats.goCalls, e.ssaStats.hosts, e.ssaStats.entries
	if _, err := r.Run(compileForTest(t, "run(300)")); err != nil {
		t.Fatal(err)
	}
	calls, hosts, entries = e.ssaStats.goCalls-calls, e.ssaStats.hosts-hosts, e.ssaStats.entries-entries
	// A call of Go with a frame for the join alone.
	if entries == 0 || calls > 1 || hosts != 0 {
		t.Errorf("run ran natively %d times, made %d calls of Go with frames and left its code %d times", entries, calls, hosts)
	}
}
