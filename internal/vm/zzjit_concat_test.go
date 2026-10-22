//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"runtime"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
)

// concatSetup's loops make concats of more parts than a call of Go takes
// (abi.MaxGoArgs): a chain of five calls and the strings between them,
// which the compiler joins in one concat of ten parts, and a template of
// 41; the template again with BigInts among its parts, which Go converts,
// and with an object among its parts whose toString
// counts its calls, and with a symbol, which throws; a template's parts
// converted in their order, objects' toString logging it among a part's
// own effect -- partOrder's compiled for primitives first, then given
// objects -- and a template of primitives of every kind.
const concatSetup = `
var calls = 0;
var obj = { toString() { calls++; return "O" } };
function chainLoop(xs, n) { let h = 0, out = ''; for (let i = 0; i < n; i++) { const x = xs[i % xs.length]; out = '' + ':' + String(x) + ':' + String(x) + ':' + String(x) + ':' + String(x) + ':' + String(x); h = (h * 31 + out.length) | 0 } return h + '|' + out }
function chains(n) { return chainLoop([1.5, '42', true, null, undefined, NaN, -0, '0x10', 1e21, ' 7px'], n) }
function templateLoop(xs, n) { let h = 0, out = ''; for (let i = 0; i < n; i++) { const x0 = xs[i % xs.length], x1 = i, x2 = i * 0.5, x3 = xs[(i + 3) % xs.length]; out = ` + "`" + `${x0}-${x1}-${x2}-${x3}-${x0}-${x1}-${x2}-${x3}-${x0}-${x1}-${x2}-${x3}-${x0}-${x1}-${x2}-${x3}-${x0}-${x1}-${x2}-${x3}-` + "`" + `; h = (h * 31 + out.length) | 0 } return h + '|' + out }
function templates(n) { return templateLoop(['a', 2, false, null, undefined, 'long string here', -0, 1.5], n) }
function bigints(n) { return templateLoop([1n, 'b', 2n ** 70n, 'c'], n) }
function objects(n) { calls = 0; let r = templateLoop(['a', 'b', obj, 'c'], n); return r + '|' + calls }
function orderLoop(n) { const log = []; let out = ''; const o1 = { toString() { log.push('o1'); return 'A' } }, o2 = { toString() { log.push('o2'); return 'B' } }; for (let i = 0; i < n; i++) { log.length = 0; out = ` + "`" + `${o1}${(log.push('mid'), 'm')}${o2}:${i}` + "`" + `; } return out + '|' + log.join(',') }
function primLoop(n) { let h = 0, out = ''; for (let i = 0; i < n; i++) { const v = [i, i * 0.25, true, null, undefined, 'str', -0, 1e21][i % 8]; out = ` + "`" + `<${v}|${i}|${v}>` + "`" + `; h = (h * 31 + out.length) | 0 } return h + '|' + out }
function singleLoop(n) { let out = '', t = ''; for (let i = 0; i < n; i++) { const s = ` + "`" + `${i * 2}` + "`" + `; t = typeof s; out = s + 1 } return t + ':' + out }
function chainOrder(n) { const log = []; let out = ''; const o1 = { valueOf() { log.push('o1'); return 'A' }, toString() { return 'X' } }, o2 = { valueOf() { log.push('o2'); return 'B' } }; for (let i = 0; i < n; i++) { log.length = 0; out = ':' + o1 + (log.push('mid'), 'm') + o2 + i } return out + '|' + log.join(',') }
var orderLog = [];
var lo1 = { toString() { orderLog.push('o1'); return 'A' } }, lo2 = { toString() { orderLog.push('o2'); return 'B' } };
function partOrder(a, b, n) { let out = ''; for (let i = 0; i < n; i++) { orderLog.length = 0; out = ` + "`" + `${a}${(orderLog.push('mid'), 'm')}${b}:${i}` + "`" + ` } return out + '|' + orderLog.join(',') }
function partOrders(n) { partOrder(1, 2, n); partOrder('x', true, n); return partOrder(lo1, lo2, n) }
function symbols() { try { return templateLoop(['a', 'b', 'c', Symbol('s')], 4) } catch (e) { return e.constructor.name } }
`

// A concat of any number of parts is joined by native code's calls of Go
// (ssa.GoConcat), a call's operands at a time, each call's string the next
// one's first part: one of more than eight had left native code at every
// turn of its loop. A part's conversion -- a template's to_string, a
// chain's ” + part -- whose string the concat alone reads is a check that
// it is a primitive (ir.StringPart), which the concat converts: no call of
// Go of its own. Each answer is Node's (v26.10.0) and the interpreter's;
// warm, the loops of primitives run natively, make one call of Go with a
// frame at most in a round -- a method read once by Go -- and never leave
// their code, and their code joins with the concats' calls alone. An
// object among the parts fails the check, and Go converts it there,
// before the parts after it are computed -- partOrder compiled for
// primitives, then given objects that log their conversions -- and a
// template of one part, which no concat reads, stays a string.
func TestJITLongConcats(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	runtime.GC()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	rounds := [][2]string{
		{"chains(30)", "-1566992171|: 7px: 7px: 7px: 7px: 7px"},
		{"templates(30)", "-303875368|long string here-29-14.5-a-long string here-29-14.5-a-long string here-29-14.5-a-long string here-29-14.5-a-long string here-29-14.5-a-"},
		{"bigints(30)", "1348457046|b-29-14.5-1-b-29-14.5-1-b-29-14.5-1-b-29-14.5-1-b-29-14.5-1-"},
		{"objects(30)", "-106054026|b-29-14.5-a-b-29-14.5-a-b-29-14.5-a-b-29-14.5-a-b-29-14.5-a-|70"},
		{"symbols()", "TypeError"},
		{"orderLoop(30)", "AmB:29|o1,mid,o2"},
		{"primLoop(30)", "1509837788|<str|29|str>"},
		{"singleLoop(30)", "string:581"},
		{"chainOrder(30)", ":AmB29|o1,mid,o2"},
		{"partOrders(30)", "AmB:29|o1,mid,o2"},
	}
	// Primitives first, which the code is compiled for; then what makes
	// its checks fail.
	var srcs []string
	for range 6 {
		for _, rd := range rounds {
			if rd[0] == "chains(30)" || rd[0] == "templates(30)" || rd[0] == "primLoop(30)" || rd[0] == "singleLoop(30)" {
				srcs = append(srcs, rd[0])
			}
		}
		srcs = append(srcs, "partOrder(1, 2, 30)")
	}
	r := jitAgreesForTest(t, concatSetup, srcs...)
	for _, c := range [][2]string{{"chainLoop", "chains(30)"}, {"templateLoop", "templates(30)"}, {"primLoop", "primLoop(30)"}, {"partOrder", "partOrder(1, 2, 30)"}} {
		e := jitNativeForTest(t, r, c[0])
		calls, hosts, entries := e.ssaStats.goCalls, e.ssaStats.hosts, e.ssaStats.entries
		if _, err := r.Run(compileForTest(t, c[1])); err != nil {
			t.Fatal(err)
		}
		calls, hosts, entries = e.ssaStats.goCalls-calls, e.ssaStats.hosts-hosts, e.ssaStats.entries-entries
		if entries == 0 || calls > 1 || hosts != 0 {
			t.Errorf("%s ran natively %d times, made %d calls of Go with frames and left its code %d times", c[0], entries, calls, hosts)
		}
	}
	// A part's conversion the concat that reads it makes is no call of Go
	// of its own (compile's stringParts): the template's 41 parts are three
	// calls of sixteen operands at most, the chain's ten one -- and the
	// return's h + '|' + out one -- with no one part's concat nor a chain's
	// '' + part among them.
	for _, c := range []struct {
		name  string
		calls int
	}{{"templateLoop", 4}, {"chainLoop", 2}, {"primLoop", 2}} {
		concats, others := 0, 0
		for _, st := range jitNativeForTest(t, r, c.name).ssa.GoCallStates() {
			switch ssa.GoOp(st.Op & 0xff) {
			case ssa.GoConcat:
				if st.Op>>abi.GoOpArgcShift&0xff < 2 {
					others++
				} else {
					concats++
				}
			case ssa.GoAdd:
				others++
			}
		}
		if concats != c.calls || others != 0 {
			t.Errorf("%s's code joins with %d concats' calls, want %d, and makes %d conversions of parts", c.name, concats, c.calls, others)
		}
	}
	want := New(Config{})
	defer func() { want.Close(); want.ReleaseClosed() }()
	if _, err := want.Run(compileForTest(t, concatSetup)); err != nil {
		t.Fatal(err)
	}
	for _, rd := range rounds {
		for _, rt := range []*Runtime{want, r} {
			v, err := rt.Run(compileForTest(t, "''+("+rd[0]+")"))
			if err != nil {
				t.Fatalf("%s: %v", rd[0], err)
			}
			if got := v.String().Go(); got != rd[1] {
				t.Errorf("%s = %s, want Node's %s", rd[0], got, rd[1])
			}
		}
	}
}

// addSetup's loop makes +'s of strings and numbers one at a time, each a
// call of Go from native code (ssa.GoAdd): a string and a number either
// way round, a number and the empty string, two strings -- numbers of
// every kind of string (-0, past 1023, past 2^53, 1e21, a fraction, NaN,
// the infinities) and strings of one byte, of two-byte units and long.
const addSetup = `
var nums = [0, -0, 7, -7, 1023, 1024, 2 ** 53 - 1, 2 ** 53, 1e21, 0.5, -1.5, NaN, Infinity, -Infinity, 123456789012]
var strs = ['', 'ab', 'é', '日本', 'x'.repeat(30)]
function addLoop(n) { let h = 0, a = '', b = '', c = '', d = '', e = ''; for (let i = 0; i < n; i++) { const x = nums[i % nums.length], s = strs[i % strs.length]; a = s + x; b = x + s; c = x + ''; d = '' + x; e = s + s; h = (h * 31 + a.length + b.length * 3 + c.length * 5 + d.length * 7 + e.length * 11 + a.charCodeAt(0) + b.charCodeAt(b.length - 1)) | 0 } return h + ':' + a + ',' + b + ',' + c + ',' + d + ',' + e }
`

// Native code's + of two strings, or of a string and a number, is the
// concatenation Go makes without add's conversions (jitConcatAdd): each
// answer is Node's (v26.10.0) and the interpreter's, and the loop's code
// makes its +'s by calls of Go.
func TestJITConcatAdd(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	var rounds []string
	for range 6 {
		rounds = append(rounds, "addLoop(300)", "addLoop(301)", "addLoop(17)")
	}
	r := jitAgreesForTest(t, addSetup, rounds...)
	e := jitNativeForTest(t, r, "addLoop")
	adds := 0
	for _, st := range e.ssa.GoCallStates() {
		if ssa.GoOp(st.Op&0xff) == ssa.GoAdd {
			adds++
		}
	}
	if adds == 0 {
		t.Error("addLoop's code makes no + by a call of Go")
	}
	for src, want := range map[string]string{
		"addLoop(300)": "129195200:" + strings.Repeat("x", 30) + "123456789012,123456789012" + strings.Repeat("x", 30) + ",123456789012,123456789012," + strings.Repeat("x", 60),
		"addLoop(301)": "-289915984:0,0,0,0,",
		"addLoop(17)":  "-1295841136:ab0,0ab,0,0,abab",
	} {
		v, err := r.Run(compileForTest(t, src))
		if err != nil || v.String().Go() != want {
			t.Errorf("%s = %v, %v; want Node's %s", src, v, err, want)
		}
	}
}
