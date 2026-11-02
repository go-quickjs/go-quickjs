package vm

import "testing"

// emptyAddTestSrc converts values of every kind to strings by "" + x of a
// local (zzz_emptyadd.go): stored back to the local, to it and then read
// (set_local_get), to a new binding (init_local) and to another local,
// returned, and in a + chain and a template, whose join takes the
// addition's operand as it is; strings flat, empty, past ASCII and a
// rope, numbers, a boolean, undefined, null, a BigInt, objects whose
// valueOf, toString and Symbol.toPrimitive are counted, an array, a
// String object and a symbol, which throws. emptyAddTestWant is Node's
// answer (v26.10.0).
const emptyAddTestSrc = `var calls = 0
var vals = ['s', '', 'café', 42, -0, 1.5, NaN, true, undefined, null, 10n, { valueOf() { calls++; return 7 }, toString() { calls++; return 'o' } },
  { toString() { calls++; return 'ts' } }, [1, 2], new String('w'), { [Symbol.toPrimitive](h) { calls++; return h } }, Symbol('y')]
function g(f) { try { return f() } catch (e) { return e.constructor.name } }
function set1(x) { let _k = 0; x = '' + x; return x }
function set2(x) { let _k = 0; x = '' + x; _k = x; return typeof _k + ':' + _k }
function init1(x) { let _k = 0; let y = '' + x; return y + '/' + typeof y }
function ret1(x) { let _k = 0; return '' + x }
function other(x) { let _k = 0, y = 'z'; y = '' + x; return y + x }
function chain(x) { let _k = 0; return 'a' + x + 'b' + x }
function tmpl(x) { let _k = 0; return ` + "`" + `[${x}|${x}]` + "`" + ` }
function rope(x) { let _k = 0; var r = x + x; r = '' + r; return r.length }
function all() {
  calls = 0
  var o = []
  for (var v of vals) {
    o.push(g(() => set1(v)) + ',' + g(() => set2(v)) + ',' + g(() => init1(v)) + ',' + g(() => ret1(v)) + ',' + g(() => other(v)) + ',' +
      g(() => chain(v)) + ',' + g(() => tmpl(v)))
  }
  o.push(rope('ab'), rope('é'))
  return o.join(' ') + ' calls=' + calls
}
function loop(n) { let _k = 0; var s = 0, t = ''; for (var i = 0; i < n; i++) { var x = vals[i % 4]; x = '' + x; s += x.length; t = '' + x } return s + t }
all() + ' | ' + loop(100)
`

const emptyAddTestWant = `s,string:s,s/string,s,ss,asbs,[s|s] ,string:,/string,,,ab,[|] café,string:café,café/string,café,cafécafé,acafébcafé,[café|café] 42,string:42,42/string,42,4242,a42b42,[42|42] 0,string:0,0/string,0,00,a0b0,[0|0] 1.5,string:1.5,1.5/string,1.5,1.51.5,a1.5b1.5,[1.5|1.5] NaN,string:NaN,NaN/string,NaN,NaNNaN,aNaNbNaN,[NaN|NaN] true,string:true,true/string,true,truetrue,atruebtrue,[true|true] undefined,string:undefined,undefined/string,undefined,undefinedundefined,aundefinedbundefined,[undefined|undefined] null,string:null,null/string,null,nullnull,anullbnull,[null|null] 10,string:10,10/string,10,1010,a10b10,[10|10] 7,string:7,7/string,7,77,a7b7,[o|o] ts,string:ts,ts/string,ts,tsts,atsbts,[ts|ts] 1,2,string:1,2,1,2/string,1,2,1,21,2,a1,2b1,2,[1,2|1,2] w,string:w,w/string,w,ww,awbw,[w|w] default,string:default,default/string,default,defaultdefault,adefaultbdefault,[string|string] TypeError,TypeError,TypeError,TypeError,TypeError,TypeError,TypeError 4 2 calls=30 | 17542`

// "" + x of a local is x itself where it is a string, and the addition
// otherwise, in the tree tier and in the interpreter, each answer Node's.
func TestEmptyStringAdd(t *testing.T) {
	for _, tree := range []bool{true, false} {
		SetTreeTier(tree)
		r := New(Config{})
		v, err := r.Run(compileForTest(t, emptyAddTestSrc))
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got := v.String().Go(); got != emptyAddTestWant {
			t.Errorf("tree tier %v:\ngot  %s\nwant %s", tree, got, emptyAddTestWant)
		}
	}
	SetTreeTier(true)
}
