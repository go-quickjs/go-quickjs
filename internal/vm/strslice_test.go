package vm

import "testing"

// A slice of one code unit -- substring, slice and substr -- is the
// runtime's string of it, as s[i] is (unitString), and every kind answers
// as before: ASCII, a quote, a backslash and a newline, Latin-1, a
// character past it, each half of a surrogate pair, either order of
// substring's arguments, a negative start. And strings compare equal by
// content whatever their form -- flat or a rope, one character or more, a
// literal or a slice -- as String.Equals compares two flat ones in place.
// Each answer is Node's, three rounds alike.
func TestOneCharSlices(t *testing.T) {
	const src = `
		const out = []
		const s = 'a"\\\né€😀z'
		for (let round = 0; round < 3; round++) {
			const r = []
			for (let i = 0; i < s.length; i++) {
				const a = s.substring(i, i + 1), b = s.slice(i, i + 1), c = s.substr(i, 1), d = s.substring(i + 1, i)
				r.push([a.length, a.charCodeAt(0), a === b, b === c, c === d, a === s[i], a === s.charAt(i)].join(':'))
			}
			r.push(s.slice(-1), s.substr(-1, 1), s.substring(9, 8), s.substr(8, 5), s.slice(2, 3) === '\\')
			let rope = ''
			for (let i = 0; i < 3; i++) rope += 'ab'
			const flat = 'ababab'
			r.push(rope === flat, flat === rope, (rope + 'x') === 'abababx', 'x' === 'y', '"' === s.substring(1, 2), 'é' === s.substring(4, 5), '€' === s[5])
			r.push(s.substring(6, 7) === '\ud83d', s.substring(7, 8) === '\ude00', s.substring(6, 8) === '😀', 'ab' === 'a' + 'b', '' === s.substring(3, 3))
			out.push(r.join(' '))
		}
		out[0] + '\n' + (out[0] === out[1] && out[1] === out[2])`
	const want = "1:97:true:true:true:true:true 1:34:true:true:true:true:true 1:92:true:true:true:true:true 1:10:true:true:true:true:true 1:233:true:true:true:true:true 1:8364:true:true:true:true:true 1:55357:true:true:true:true:true 1:56832:true:true:true:true:true 1:122:true:true:true:true:true z z z z true true true true false true true true true true true true true\ntrue"
	r := New(Config{})
	defer r.Close()
	v, err := r.Run(compileForTest(t, src))
	if err != nil {
		t.Fatal(err)
	}
	if got := v.String().Go(); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}
