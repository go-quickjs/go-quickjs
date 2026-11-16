package quickjs_test

import (
	"os"
	"strings"
	"testing"

	quickjs "github.com/go-quickjs/go-quickjs"
)

// The scripts in testdata/cursor_*.js and testdata/async_first_run.js,
// each run as a classic script, give what Node 26 gives them
// (vm.runInThisContext): for-in loops left by a break, a continue, a
// return or a throw, and loops after them, whose cursors the register
// interpreter hands from one loop to the next; destructurings and for-of
// loops over arrays, whose cursors it releases at their close and takes
// again; the answer for the keys up a prototype chain, which it keeps
// while the chain's validity cell holds; and async functions, whose first
// run it makes as a call, the generator made at the first await.
func TestCursorReuseMatchesNode(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ file, want string }{
		{"cursor_forin.js", `{"breaks":"abaabacaabaabacaabaabaca","finallyContinue":"abcabc","returns":"abbbcb",` +
			`"labeled":"0a,1a,2a,a,b","chain":"a,az,a,a0,a,atoString,a,aq,a,01w,01","throws":"acacab",` +
			`"gen":"aaabbctrue","deleted":"aaab","dict":"abc,abc,abce,bce,ce,bce,bcea,bca,bcaf,7bcaf,bcaf,x,bcaf,x"}`},
		{"cursor_destructure.js", `{"nested":"1,2,3,4,7,,7,8,1,2,3,4,7,,7,8,1,2,3,4,7,,7,8","doneThenGrow":"1,0,,1,78",` +
			`"throwPath":"TypeError,0,1,TypeError,1,2","forOfBreak":"1,2,11,21,31,3,1,2,11,21,31,3",` +
			`"gen":"y,1,2,9,8,7,4:5","holes":"1,P,3,1,undefined","returnMethod":"rettrue,rettrue|1,1,2,undefined,1",` +
			`"setterTarget":"x1,30,x2,60,x3,90","shrink":"1,0,undefined,1,9,3,4"}`},
	} {
		src, err := os.ReadFile("testdata/" + c.file)
		if err != nil {
			t.Fatal(err)
		}
		rt := quickjs.New()
		// Twice, the second run's loops finding the first's cursors.
		for run := 0; run < 2; run++ {
			if got := evalString(t, rt, string(src)); got != c.want {
				t.Errorf("%s, run %d:\n got %s\nwant %s", c.file, run, got, c.want)
			}
		}
		rt.Close()
	}
}

// Async functions run to their first await at the call, in the order
// Node gives: what each logs, the jobs their awaits and their promises
// queue, a thenable returned, a closure made before an await that reads
// a binding the function changes after it, a sloppy this, arguments, a
// default parameter, a method's this, a recursion of async calls that
// never await.
func TestAsyncFirstRunOrder(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("testdata/async_first_run.js")
	if err != nil {
		t.Fatal(err)
	}
	const want = `["main","a1 start","after a1","a2 start","after a2","a3 start","after a3","a4","after a4",` +
		`"a5 4","a6 caught","object","a9 12undefined","m 3","main end","a1 start","a2 after","then","p1 1",` +
		`"p3 x","p7 2","tick1","a1 start","p2 2","p4 5","p5 7","p6 6","p8 3","p9 3","pm 3","tick2",` +
		`"a1 start","p10 11","outer inin","tick3","sum 3","rec deep"]`
	rt := quickjs.New()
	defer rt.Close()
	for run := 0; run < 2; run++ {
		evalString(t, rt, string(src))
		if got := evalString(t, rt, `JSON.stringify(log)`); got != want {
			t.Errorf("run %d:\n got %s\nwant %s", run, got, want)
		}
	}
}

// An async function that throws in its first run, before any await, or
// after one, rejects its promise, which nothing takes: reported once each.
func TestAsyncFirstRunRejection(t *testing.T) {
	t.Parallel()
	rt := quickjs.New()
	defer rt.Close()
	var seen []string
	rt.OnUnhandledRejection(func(reason quickjs.Value) {
		msg, _ := reason.Get("message")
		seen = append(seen, msg.String())
	})
	src := `async function now() { throw new Error("now") }
		async function later() { await 0; throw new Error("later") }
		async function taken() { throw new Error("taken") }
		now(); later(); taken().catch(() => {});`
	if _, err := rt.Eval(src); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(seen, ","), "now,later"; got != want {
		t.Errorf("reported %q, want %q", got, want)
	}
	// A recursion of async calls that never await, too deep for the
	// stack: its RangeError rejects the innermost call's promise, which
	// the outer calls resolve with, as Node's.
	got := evalString(t, rt, `var r; async function deep(n) { return deep(n + 1) }
		deep(0).catch(e => { r = e.constructor.name }); 0`)
	_ = got
	if got := evalString(t, rt, `String(r)`); got != "RangeError" {
		t.Errorf("deep recursion: %s, want RangeError", got)
	}
}
