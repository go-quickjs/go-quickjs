package vm

import "testing"

// TestMatchResultStrings pins exec's arrays where the matched text is written
// into the array's own allocation and cut from the subject's bytes, on both
// tiers: text that is not ASCII, a match ending or starting between the
// halves of a pair -- which must still join with the other half -- groups,
// indices, named groups, an array changed after the match, and lastIndex
// past non-ASCII text. The answers are Node's.
func TestMatchResultStrings(t *testing.T) {
	defer SetTreeTier(true)
	const src = `function show(m) { return m === null ? "null" : JSON.stringify([m.slice(), m.index, m.length, m.groups, m.indices || null]); }
function f() {
  var out = [];
  out.push(show(/quick ᶠᵒˣ/.exec("the quick ᶠᵒˣ jumped")));
  out.push(show(/😀b/u.exec("a😀b")));
  var lo = /\uDE00/.exec("a😀"); out.push(show(lo), lo[0].charCodeAt(0));
  var hi = /x\uD83D/.exec("ᵃx😀"); out.push(show(hi), (hi[0] + "\uDE00") === "x😀", (hi[0] + "\uDE00").length);
  var l2 = /\uDE00y/.exec("ᵃ😀y"); out.push(("\uD83D" + l2[0]) === "😀y");
  out.push(show(/(ᵈ)(x)?/.exec("aᵈ")));
  out.push(show(/ᵒ(ˣ)/d.exec("ᶠᵒˣ")));
  out.push(show(/(?<a>ᵒ)/.exec("ᶠᵒˣ")));
  var m = /a/.exec("ba"); m.push("x"); m.foo = 1; m[0] += "!"; out.push(JSON.stringify(m), m.length, m.foo, m.index);
  var s = "ᵃᵇᶜ-abc-ᵃᵇᶜ", r = [];
  for (var i = 0; i < 3; i++) { var re = /ᵇ(.)/g; re.lastIndex = i * 4; var mm = re.exec(s); r.push(mm && mm[0] + mm[1] + mm.index); }
  out.push(r.join());
  out.push(JSON.stringify("ᶠᵒˣ😀ᵈ".slice(1, 4)), "ᶠᵒˣ😀ᵈ".substring(3, 5).length, "a😀".substring(1, 2).charCodeAt(0));
  return out.join("\n");
}
f()`
	const want = `[["quick ᶠᵒˣ"],4,1,null,null]
[["😀b"],1,1,null,null]
[["\ude00"],2,1,null,null]
56832
[["x\ud83d"],1,1,null,null]
true
3
true
[["ᵈ","ᵈ",null],1,3,null,null]
[["ᵒˣ","ˣ"],1,2,null,[[1,3],[2,3]]]
[["ᵒ","ᵒ"],1,2,{"a":"ᵒ"},null]
["a!","x"]
2
1
1
ᵇᶜᶜ1,ᵇᶜᶜ9,ᵇᶜᶜ9
"ᵒˣ\ud83d"
2
55357`
	for _, on := range []bool{false, true} {
		SetTreeTier(on)
		fn := compileForTest(t, src)
		r := New(Config{})
		v, err := r.Run(fn)
		if err != nil {
			r.Close()
			t.Fatalf("tree %v: %v", on, err)
		}
		got, err := r.ToString(v)
		if err != nil || got.Go() != want {
			t.Errorf("tree %v: got %v, %v; want %s", on, got, err, want)
		}
		r.Close()
	}
}

// TestReplaceShortResults pins replace and match of the built-in exec, whose
// matches are kept in the caller's frame, and replace's short ASCII results,
// written with their String in one allocation, on both tiers: results of 0,
// 71, 72 and 73 bytes, a template with $, text that is not ASCII, empty
// matches, and lastIndex and the legacy statics after. The answers are Node's.
func TestReplaceShortResults(t *testing.T) {
	defer SetTreeTier(true)
	const src = `function f() {
  var out = [], s = "the quick abc brown fox jumped abc over the lazy dog";
  out.push(s.replace(/abc /g, "-"), s.replace(/abc /g, ""), s.replace(/abc /, "-"));
  out.push("abcabc".replace(/abc/g, ""), "".replace(/x*/g, "-"), "aaa".replace(/a*?/g, "-"));
  out.push(s.replace(/o/g, "0000000000"), s.replace(/o/g, "00").length);
  out.push(s.replace(/(abc) /g, "[$1]"), s.replace(/abc /g, "ᵃ"), "ᵃbcᵃ".replace(/ᵃ/g, "a"));
  var long = "x".repeat(70) + "abc";
  out.push(long.replace(/abc/g, "d").length, long.replace(/abc/g, "de").length, long.replace(/abc/g, "def").length);
  out.push("a.b.c".replace(/\./g, "/"), s.match(/o\w/g).join(), "zz".match(/a/g));
  var re = /b/g; re.lastIndex = 5; out.push("abcb".replace(re, "X"), re.lastIndex, RegExp.lastMatch, RegExp.leftContext);
  return out.join("|");
}
f()`
	const want = `the quick -brown fox jumped -over the lazy dog|the quick brown fox jumped over the lazy dog|the quick -brown fox jumped abc over the lazy dog||-|-a-a-a-|the quick abc br0000000000wn f0000000000x jumped abc 0000000000ver the lazy d0000000000g|56|the quick [abc]brown fox jumped [abc]over the lazy dog|the quick ᵃbrown fox jumped ᵃover the lazy dog|abca|71|72|73|a/b/c|ow,ox,ov,og||aXcX|0|b|abc`
	for _, on := range []bool{false, true} {
		SetTreeTier(on)
		fn := compileForTest(t, src)
		r := New(Config{})
		v, err := r.Run(fn)
		if err != nil {
			r.Close()
			t.Fatalf("tree %v: %v", on, err)
		}
		got, err := r.ToString(v)
		if err != nil || got.Go() != want {
			t.Errorf("tree %v: got %v, %v; want %s", on, got, err, want)
		}
		r.Close()
	}
}
