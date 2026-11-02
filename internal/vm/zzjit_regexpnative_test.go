//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"strings"
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
