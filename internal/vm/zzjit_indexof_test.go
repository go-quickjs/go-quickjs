//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import "testing"

// indexOfSetup's functions call indexOf, lastIndexOf and includes on
// strings, as Octane Gameboy's base64_decode looks each character of its
// data up in its alphabet -- one-character needles and longer ones, UTF-16
// strings, ropes, positions of every kind, a number as the needle -- and
// on arrays, of numbers, NaN and -0 among them, of objects, with holes;
// and then what native code leaves to Go: a needle whose toString is
// called, a position whose valueOf is, a regular expression given to
// includes, an index on Array.prototype, the method replaced.
const indexOfSetup = `
	var alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/="
	var data = "SGVsbG8sIHdvcmxkIQ=="
	function decode() { let s = 0; for (let i = 0; i < data.length; i++) s = s * 3 + alphabet.indexOf(data.charAt(i)) | 0; return s }
	var wide = "αβγ😀δε"
	function inWide(t) { return wide.indexOf(t) + ':' + wide.includes(t) }
	function longer(t) { return alphabet.indexOf(t) }
	function from(t, i) { return alphabet.indexOf(t, i) }
	function rope(n) { let s = ''; for (let i = 0; i < n; i++) s += 'ab'; s += 'c'; return s.indexOf('c') + ':' + s.indexOf('ba', 3) }
	function num(x) { return '12345'.indexOf(x) }
	function has(t) { return alphabet.includes(t) }
	function hasFrom(t, i) { return alphabet.includes(t, i) }
	var called = 0
	var obj = { toString() { called++; return 'Q' } }
	function onObj() { return alphabet.indexOf(obj) + ':' + called }
	function onRegExp() { try { return alphabet.includes(/A/) } catch (e) { return e.constructor.name } }
	function last(t) { return alphabet.lastIndexOf(t) + ':' + 'abcabc'.lastIndexOf(t, 3) + ':' + 'abcabc'.lastIndexOf(t, -1) + ':' + 'abcabc'.lastIndexOf(t, NaN) }
	var nums = [3, 1, 4, 1, 5, 9, 2, 6, NaN, -0]
	var o1 = { k: 1 }, o2 = { k: 2 }
	var objs = [o1, o2, 'o1', 2, o1]
	var holey = [1, , 3, , undefined, 1]
	function arrIndexOf(x) { return nums.indexOf(x) + ':' + nums.lastIndexOf(x) + ':' + nums.includes(x) }
	function arrFrom(x, i) { return nums.indexOf(x, i) + ':' + nums.lastIndexOf(x, i) + ':' + nums.includes(x, i) }
	function arrObjs(x) { return objs.indexOf(x) + ':' + objs.lastIndexOf(x) + ':' + objs.includes(x) }
	function arrHoles(x) { return holey.indexOf(x) + ':' + holey.lastIndexOf(x) + ':' + holey.includes(x) }
	function arrEmpty(x) { return [].indexOf(x) + ':' + [].includes(x) }
	var pos = { valueOf() { called++; return 2 } }
	function arrValueOf() { return nums.indexOf(1, pos) + ':' + called }
	function decodeArr() { let s = 0; for (let i = 0; i < data.length; i++) s = s * 3 + nums.indexOf(data.charCodeAt(i) & 7) | 0; return s }
`

// A string's or an array's indexOf, lastIndexOf or includes is a call of
// Go of its own natively (ssa.GoIndexOf), as V8's code calls its
// StringIndexOf and ArrayIndexOf builtins, where it had been a call of the
// built-in, its frame and its arguments made by Go. Each answer is the
// interpreter's, and decode's and decodeArr's lookups neither call a
// function of Go's nor leave their code.
func TestJITSearches(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	drivers := ""
	for _, name := range []string{"decode", "inWide", "longer", "from", "rope", "num", "has", "hasFrom", "onObj", "onRegExp",
		"last", "arrIndexOf", "arrFrom", "arrObjs", "arrHoles", "arrEmpty", "arrValueOf", "decodeArr"} {
		drivers += "function drive_" + name + "(n, a, b) { let r; for (let i = 0; i < n; i++) r = " + name + "(a, b); return r };"
	}
	rounds := []string{}
	for range 6 {
		rounds = append(rounds, `drive_decode(30)`, `drive_inWide(30, 'γ')`, `drive_inWide(30, '😀')`, `drive_inWide(30, '\uDE00')`,
			`drive_inWide(30, 'x')`, `drive_longer(30, 'xyz')`, `drive_longer(30, 'yx')`, `drive_longer(30, '')`,
			`drive_from(30, 'A', 0)`, `drive_from(30, 'A', 1)`, `drive_from(30, 'z', -5)`, `drive_from(30, '', 99)`, `drive_from(30, 'B', NaN)`,
			`drive_from(30, '=', Infinity)`, `drive_from(30, 'C', 2.7)`, `drive_from(30, 'D', '3')`,
			`drive_rope(30, 40)`, `drive_num(30, 3)`, `drive_num(30, 3.5)`, `drive_num(30, undefined)`,
			`drive_has(30, 'xyz')`, `drive_has(30, 'zyx')`, `drive_hasFrom(30, 'A', 1)`, `drive_hasFrom(30, '+', -1)`,
			`drive_onObj(30)`, `drive_onRegExp(30)`,
			`drive_last(30, 'c')`, `drive_last(30, 'bc')`, `drive_last(30, 'z')`, `drive_last(30, '')`,
			`drive_arrIndexOf(30, 1)`, `drive_arrIndexOf(30, NaN)`, `drive_arrIndexOf(30, 0)`, `drive_arrIndexOf(30, '1')`, `drive_arrIndexOf(30, 7)`,
			`drive_arrFrom(30, 1, 2)`, `drive_arrFrom(30, 1, -7)`, `drive_arrFrom(30, 4, 99)`, `drive_arrFrom(30, 3, -99)`, `drive_arrFrom(30, 1, NaN)`,
			`drive_arrObjs(30, o1)`, `drive_arrObjs(30, o2)`, `drive_arrObjs(30, 'o1')`, `drive_arrObjs(30, { k: 1 })`,
			`drive_arrHoles(30, undefined)`, `drive_arrHoles(30, 1)`, `drive_arrHoles(30, 3)`, `drive_arrEmpty(30, 1)`,
			`drive_arrValueOf(30)`, `drive_decodeArr(30)`)
	}
	rounds = append(rounds,
		// An index on Array.prototype, where holey has holes: Go's call,
		// which reads it there.
		`(Array.prototype[1] = 'p', drive_arrHoles(30, 'p') + ' ' + drive_arrHoles(30, undefined))`,
		`(delete Array.prototype[1], drive_arrHoles(30, undefined))`)
	r := jitAgreesForTest(t, indexOfSetup+drivers, rounds...)
	for _, name := range []string{"decode", "decodeArr"} {
		var e *jitEntry
		for k, x := range r.jit.cache {
			if fn := k.Value(); fn != nil && fn.Name == name {
				e = x
			}
		}
		if e == nil || e.ssa == nil {
			t.Errorf("%s has no native code: %+v", name, e)
			continue
		}
		if e.ssaStats.goCalls != 0 || e.ssaStats.hosts != 0 {
			t.Errorf("%s called Go's functions %d times, left its code %d times", name, e.ssaStats.goCalls, e.ssaStats.hosts)
		}
	}
	// indexOf replaced on String.prototype: the call's check of its
	// function fails, and the call is Go's.
	jitAgreesForTest(t, indexOfSetup+drivers, append(rounds, `(String.prototype.indexOf = function (t) { return 7 }, drive_decode(30))`)...)
}
