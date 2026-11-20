package vm

import "testing"

// String.prototype.match calls RegExp.prototype[Symbol.match] itself where
// reading it runs nothing and finds the realm's (matchIntrinsic), and asks
// for the method otherwise: one of the RegExp's own, the prototype's
// replaced, a subclass's, a lastIndex whose conversion runs code -- each
// answered as the protocol answers it, and in its order of steps.
func TestStringMatchIntrinsic(t *testing.T) {
	t.Parallel()
	r := newTestRuntime(t, Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, `
		const out = [], s = 'x=12 y=345 z=6'
		out.push(JSON.stringify(s.match(/\d+/g)), JSON.stringify(s.match(/(\d)(\d)/)), String(s.match(/q/g)))
		const re = /\d+/; re.lastIndex = 5
		const m = s.match(re); out.push(m.index, m[0], re.lastIndex)
		const g = /\d/g; g.lastIndex = 3; s.match(g); out.push(g.lastIndex)
		const own = /a/; own[Symbol.match] = function (str) { return 'own:' + str }; out.push(s.match(own))
		const li = /\d+/; li.lastIndex = { valueOf() { out.push('valueOf'); return 0 } }; out.push(s.match(li)[0])
		class R extends RegExp { [Symbol.match](str) { return 'sub:' + str.length } }
		out.push(new R('a').test('a'), s.match(new R('\d')))
		const saved = RegExp.prototype[Symbol.match]
		RegExp.prototype[Symbol.match] = function (str) { return 'proto:' + this.source }
		out.push(s.match(/\d+/g))
		RegExp.prototype[Symbol.match] = saved
		out.push(s.match(/\d+/g).length)
		const ex = RegExp.prototype.exec
		RegExp.prototype.exec = function (str) { out.push('exec'); return ex.call(this, str) }
		out.push(s.match(/z=(\d)/)[1])
		RegExp.prototype.exec = ex
		Object.defineProperty(RegExp.prototype, 'flags', { get() { out.push('flags'); return 'g' }, configurable: true })
		out.push(s.match(/\d+/g).length)
		out.join(' ')`))
	if err != nil {
		t.Fatal(err)
	}
	want := `["12","345","6"] ["12","1","2"] null 2 12 5 0 own:x=12 y=345 z=6 valueOf 12 true sub:14 proto:\d+ 3 exec 6 flags 3`
	if got := v.String().Go(); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}
