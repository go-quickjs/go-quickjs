//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
	"time"
	"weak"
)

const regexpNativeSetup = `
function run(n) {
  var out = [];
  for (var i = 0; i < n; i++) {
    out.push("a1b22c333".replace(/(\d+)/g, "<$1>"),
      /(\w+)@(\w+)\.com/.exec("mail bob@example.com now")[2],
      "x, y ,z".split(/\s*,\s*/).join("|"),
      /^(a|b)*c$/.test("ababc"), /^(a|b)*c$/.test("ababd"),
      "Hello hello".match(/(h)ello/gi).length,
      /(?<y>\d{4})-(?<m>\d\d)/.exec("on 2026-11 or")?.groups.m,
      /(a+)+b/.test("aaaaaaaaaaaaaaaab"),
      /\bfoo\b/m.test("x\nfoo"), /(\w)\1/i.exec("abBc")?.[0],
      "café cafe".replace(/caf./g, "X"));
  }
  return out.join();
}
var lone = String.fromCharCode(0xD83D), pair = String.fromCodePoint(0x1F600)
var loneRe = new RegExp(lone, "u"), loneRe16 = new RegExp(lone)
function wide(n) {
  var out = [];
  for (var i = 0; i < n; i++) {
    out.push("Grüße, Straße".replace(/(\w+)ß/g, "[$1]"), /\p{L}+/u.exec("— Ελληνικά!")[0],
      "naïve façade".split(/\s+/).length, /[à-ÿ]+/i.exec("CAFÉ É")[0], /^.$/u.test(pair), /^.$/u.test(lone + "x"),
      loneRe.test(pair), loneRe16.test(pair), loneRe.test(lone + "a"), /./u.exec(lone + pair)[0].length,
      (lone + "a" + pair).match(/./gu).length, "日本語 text 日本".match(/[一-鿿]+/g).join("|"),
      /(é)\1/i.test("éÉ"), /\bé/u.test("é"), /[^a]+$/u.exec("x" + pair + lone)[0].length);
  }
  return out.join();
}
function budget() { return /(a+)+b/.test("a".repeat(40) + "cb") }
`

// A runtime with the JIT matches its patterns with native code once they
// have been matched twice, and gets what the interpreter gets; the code is
// counted in the JIT's budget. One without the JIT makes none.
func TestJITRegExpNative(t *testing.T) {
	r := jitAgreesForTest(t, regexpNativeSetup, `run(1)`, `run(1)`, `run(3)`, `run(3)`)
	native := 0
	for _, re := range r.regexpCache {
		if re.HasNative() {
			native++
		}
	}
	if native < 8 {
		t.Fatalf("%d of %d patterns have native code", native, len(r.regexpCache))
	}
	r = jitAgreesForTest(t, regexpNativeSetup, `wide(1)`, `wide(1)`, `wide(3)`, `wide(3)`)
	wide := 0
	for _, re := range r.regexpCache {
		if re.HasNativeUTF16() {
			wide++
		}
	}
	if wide < 10 {
		t.Fatalf("%d of %d patterns have native code for UTF-16", wide, len(r.regexpCache))
	}
	if r.jit.regexpHost.Held() == 0 || r.jit.sharedBytes() < r.jit.regexpHost.Held() {
		t.Fatalf("the JIT holds %d bytes, its patterns' code %d", r.jit.sharedBytes(), r.jit.regexpHost.Held())
	}

	plain := New(Config{})
	defer plain.Close()
	if _, err := plain.Run(compileForTest(t, regexpNativeSetup+"run(3), wide(3)")); err != nil {
		t.Fatal(err)
	}
	for _, re := range plain.regexpCache {
		if re.HasNative() || re.HasNativeUTF16() {
			t.Fatalf("/%s/ has native code in a runtime without the JIT", re.Source())
		}
	}
}

// A match native code runs out of steps in throws what the interpreter's
// does.
func TestJITRegExpNativeBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("spends the step budget")
	}
	r := jitAgreesForTest(t, regexpNativeSetup, `run(2)`, `budget()`, `budget()`)
	if _, err := r.Run(compileForTest(t, `budget()`)); err == nil || !strings.Contains(err.Error(), "too complex") {
		t.Fatalf("budget() = %v, want the step budget's error", err)
	}
	for _, re := range r.regexpCache {
		if re.Source() == "(a+)+b" && !re.HasNative() {
			t.Fatal("/(a+)+b/ has no native code")
		}
	}
}

// A runtime whose patterns have native code is collected once it is closed
// and dropped: the code's cleanup reaches nothing of the runtime's, which
// had kept every such runtime -- test262's tens of thousands -- alive.
func TestJITRegExpNativeRuntimeCollected(t *testing.T) {
	var gone []weak.Pointer[Runtime]
	for range 4 {
		r := New(Config{JIT: true})
		r.jitStress.threshold = true
		if _, err := r.Run(compileForTest(t, regexpNativeSetup+"run(2), wide(2)")); err != nil {
			t.Fatal(err)
		}
		native := false
		for _, re := range r.regexpCache {
			native = native || re.HasNative() || re.HasNativeUTF16()
		}
		if !native {
			t.Fatal("no pattern has native code")
		}
		r.Close()
		r.ReleaseClosed()
		gone = append(gone, weak.Make(r))
	}
	for i := 0; i < 20; i++ {
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
		left := 0
		for _, w := range gone {
			if w.Value() != nil {
				left++
			}
		}
		if left == 0 {
			return
		}
	}
	t.Fatal("a closed runtime with native patterns is never collected")
}

// JIT runtimes on goroutines of their own run one compiled script -- the
// same literals, the same sources of the patterns it builds -- while the
// collector runs and gives closed runtimes' code back: each compiles its own
// patterns, and so its own code, in its own arena, and gets what a runtime
// without the JIT gets. A pattern's native state is its runtime's: the
// regexp cache is the runtime's, and a pattern goroutines share (Concurrent)
// has none. Run under -race.
func TestJITRegExpNativeConcurrentRuntimes(t *testing.T) {
	fn := compileForTest(t, regexpNativeSetup+`
var acc = [];
for (var i = 0; i < 40; i++) {
  var re = new RegExp("([a-z0-9]+)@([a-z]+)[.]com", i % 2 ? "i" : "im");
  acc.push(re.exec("mail bob" + (i % 7) + "@Example.com now")[2]);
  for (var k = 0; k < 10; k++) new RegExp("x" + k + "y+").test("x" + k + "yyy");
}
acc.join() + run(3) + wide(3)`)
	plain := New(Config{})
	want, err := plain.Run(fn)
	if err != nil {
		t.Fatal(err)
	}
	wantS := want.String().Go()
	plain.Close()
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				runtime.GC()
			}
		}
	}()
	var wg sync.WaitGroup
	errs := make(chan string, 16)
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for round := 0; round < 3; round++ {
				r := New(Config{JIT: true})
				for k := 0; k < 3; k++ {
					v, err := r.Run(fn)
					if err != nil {
						errs <- err.Error()
						return
					}
					if got := v.String().Go(); got != wantS {
						errs <- "differs: " + got
						return
					}
				}
				native := 0
				for _, re := range r.regexpCache {
					if re.HasNative() || re.HasNativeUTF16() {
						native++
					}
				}
				if native < 8 {
					errs <- "too few native patterns"
				}
				r.Close()
				r.ReleaseClosed()
			}
		}()
	}
	wg.Wait()
	close(stop)
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

const regexpNativeSearchSetup = `
var long = "qwertyuiop".repeat(10), pair = String.fromCodePoint(0x1F600), lo = String.fromCharCode(0xDE00)
function search(n) {
  var out = [];
  for (var i = 0; i < n; i++) {
    var lex = /\s*(?:(\d+)|(\w+)|([^\s\w]))/y, m, toks = [];
    lex.lastIndex = 0;
    while ((m = lex.exec("let x = 10 * (y + 42);")) !== null) toks.push(m[1] || m[2] || m[3]);
    var anc = /^ab/g;
    anc.lastIndex = 2;
    out.push(toks.join(" "), anc.exec("abab"), /^ab/.exec("xab"), /^(?:a|b)c/.exec("bc")[0],
      (long + "x1" + long + "y2").match(/[xy]\d/g).join(), (long + "x1").search(/x\d/), long.search(/x\d/),
      ("nNnNe NE" + long + "ne").match(/n(e)?/gi).join(), (long + "f9" + long).match(/[a-f]\d/g),
      ("😀x a😀x" + long).match(/[a😀]x/gu).join(), (pair + lo + "a" + lo).replace(/\uDE00/gu, "#"),
      "a px 5 6 7 99px 1234px".match(/\d{1,3}px/g).join(), /(\d\d)-(\d\d)-abc/.exec("12-34-abd 5 6 77-88-abc").join(),
      "q xyz a b axyz".replace(/[ab]?xyz/g, "<$&>"), "abc".replace(/x*/g, "-"), "a" + pair + "b".replace(/(?:)/gu, "|"),
      ("a" + pair + "b").replace(/(?:)/gu, "|").length, ("a" + pair + "b").replace(/(?:)/g, "|").length,
      "ab cd".match(/\b/g).length, "Grüße é x1 éé y2".match(/[xy]\d/g).join(), ("é" + long + "x1").search(/x\d/),
      "é a px 5 6 7 99px".match(/\d{1,3}px/g).join(), "aéb é".split(/(?:)/).length, /(a)|b*/.exec("cab").join(),
      "a,b , c".split(/\s*,\s*/).join("|"), /.{0,3}ab(c)?/.exec("xxxxab abc"),
      /b/y.exec("ab"), /x\d/y.test("ax1"), "ab".replace(/b/y, "#"), /ab/y.exec("aab"), /ab/y.exec("aabé"), /a(b)?c/y.test("aabc"), /\s*\d/y.test("a 1"));
  }
  return out.join(";");
}
`

// regexpNativeSearchWant is Node's search(1), run as a script.
const regexpNativeSearchWant = "let x = 10 * ( y + 42 ) ;;;;bc;x1,y2;100;-1;n,N,n,Ne,NE,ne;f9;😀x,😀x;😀#a#;" +
	"99px,234px;77-88-abc,77,88;q <xyz> a b <axyz>;-a-b-c-;a😀|b|;8;9;4;x1,y2;101;99px;5;,;a|b|c;xxxab,;;false;ab;;;false;false"

// The search native code makes around its attempts -- sticky, anchored,
// the unicode flag's advance over a pair, the units no match begins with
// passed over a word at a time and a unit at a time, the required literal,
// empty matches -- gets Node's answers and the interpreter's and the tree
// tier's, over bytes and UTF-16, with the collector idle and while it
// marks.
func TestJITRegExpNativeSearch(t *testing.T) {
	SetTreeTier(false)
	interp := New(Config{})
	v, err := interp.Run(compileForTest(t, regexpNativeSearchSetup+"search(2)"))
	interp.Close()
	SetTreeTier(true)
	if err != nil {
		t.Fatal(err)
	}
	want := regexpNativeSearchWant + ";" + regexpNativeSearchWant
	if got := v.String().Go(); got != want {
		t.Fatalf("the interpreter: %s, want Node's %s", got, want)
	}
	for _, marking := range []bool{false, true} {
		runtime.GC()
		gc := debug.SetGCPercent(-1)
		if marking {
			jitMarkingForTest(t)
		}
		r := jitAgreesForTest(t, regexpNativeSearchSetup, "search(1)", "search(1)", "search(3)", "search(1)")
		v, err := r.Run(compileForTest(t, "search(1)"))
		if err != nil || v.String().Go() != regexpNativeSearchWant {
			t.Fatalf("marking %v: search(1) = %v %v, want Node's %s", marking, v, err, regexpNativeSearchWant)
		}
		ascii, wide := 0, 0
		for _, re := range r.regexpCache {
			if re.HasNative() {
				ascii++
			}
			if re.HasNativeUTF16() {
				wide++
			}
		}
		if ascii < 15 || wide < 4 {
			t.Errorf("marking %v: %d patterns have native code over bytes, %d over UTF-16", marking, ascii, wide)
		}
		debug.SetGCPercent(gc)
	}
}
