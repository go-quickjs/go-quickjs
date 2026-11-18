package vm

import (
	"regexp"
	"testing"
)

// savedStringCallLegacySource calls a sloppy method named charCodeAt with
// an argument evaluated after the method's read.
const savedStringCallLegacySource = `
var o={charCodeAt:function method(x){return method.arguments.length+":"+method.arguments[0]}};
function legacy(n){let result;for(let i=0;i<20;i++)result=o.charCodeAt(n+0);return ''+result}`

func TestSavedStringCallLegacyArguments(t *testing.T) {
	r := New(Config{NodeQuirks: true})
	defer r.Close()
	program := compileForTest(t, savedStringCallLegacySource+`;legacy(42)`)
	for range 3 {
		v, err := r.Run(program)
		if err != nil || v.String().Go() != "1:42" {
			t.Fatalf("got %v, %v; want 1:42", v, err)
		}
	}
}

func TestSavedStringCallDeepOverride(t *testing.T) {
	r := New(Config{})
	defer r.Close()
	program := compileForTest(t, `
var o={charCodeAt:function recurse(n){if(n===0)return 0;let result=o.charCodeAt(n-1);return result+1}};
function deep(n){let result=o.charCodeAt(n+0);return result}
''+deep(200)`)
	for range 3 {
		v, err := r.Run(program)
		if err != nil || v.String().Go() != "200" {
			t.Fatalf("got %v, %v; want 200", v, err)
		}
	}
}

// savedStringCalls are calls of a string method whose argument is
// evaluated after the method is read (call_method_w), each with what the
// specification and Node give: the method read once and before the
// argument, a getter's this, a throw's order, the saved method called
// even where the argument replaced it, the argument converted once, and
// the built-in on what is not a string or not a number.
var savedStringCalls = []struct{ name, source, want string }{
	{"updates", `function f(s) { let i=0, a=s.charCodeAt(++i), b=s.charCodeAt(i++), c=s.charCodeAt(i+1); return [a,b,c,i].join() } f("abcde")`, "98,98,100,2"},
	{"getter before increment", `function f() { let i=0, s={get charCodeAt(){i=2;return function(x){return x}}}; let a=s.charCodeAt(++i); return [a,i].join() } f()`, "3,3"},
	{"getter preserves receiver", `function f() { let i=0, s, original; original=s={get charCodeAt(){s="changed";return function(x){return this===original ? x : -1}}}; let a=s.charCodeAt(++i); return [a,i,s].join() } f()`, "1,1,changed"},
	{"getter throws before argument", `function f() { let i=0,s={get charCodeAt(){throw new RangeError("get")}};try{s.charCodeAt(++i)}catch(e){return e.name+":"+i} } f()`, "RangeError:0"},
	{"prototype getter", `function f() { let n=0,saved=String.prototype.charCodeAt,i=0;Object.defineProperty(String.prototype,"charCodeAt",{configurable:true,get(){n++;return saved}});let a="abc".charCodeAt(++i);Object.defineProperty(String.prototype,"charCodeAt",{configurable:true,writable:true,value:saved});return [a,n].join() } f()`, "98,1"},
	{"non callable after argument", `function f() { let i=0,s={charCodeAt:1};try{s.charCodeAt(++i)}catch(e){return e.name+":"+i} } f()`, "TypeError:1"},
	{"null before argument", `function f() { let i=0,s=null;try{s.charCodeAt(++i)}catch(e){return e.name+":"+i} } f()`, "TypeError:0"},
	{"argument replaces method", `function f() { let s="abc",saved=String.prototype.charCodeAt,i={valueOf(){String.prototype.charCodeAt=function(){return 7};return 0}},a=s.charCodeAt(++i);String.prototype.charCodeAt=saved;return [a,i].join() } f()`, "98,1"},
	{"argument replaces receiver", `function f() { let s="abc",i={valueOf(){s="xyz";return 0}};let a=s.charCodeAt(++i);return [a,s].join() } f()`, "98,xyz"},
	{"argument reads getter once", `function f() { let n=0,s={get charCodeAt(){n++;return function(x){return x}}},i={valueOf(){return 2}};let a=s.charCodeAt(i+1);return [a,n].join() } f()`, "3,1"},
	{"argument conversion once", `function f() { let n=0,s="abc",i={valueOf(){n++;return 0}};let a=s.charCodeAt(i+1);return [a,n].join() } f()`, "98,1"},
	{"argument converted by the call", `function f() { let n=0,s="abc",i={valueOf(){n++;return 1}};let a=s.charCodeAt(i);let b=s.charCodeAt([i][0]);return [a,b,n].join() } f()`, "98,98,2"},
	{"argument a string", `function f() { let s="abc",i=0;return [s.charCodeAt(""+(i+1)),s.charCodeAt("x"+i),s.charCodeAt(void i)].join() } f()`, "98,97,97"},
	{"argument a symbol", `function f() { let s="abc",i=Symbol();try{s.charCodeAt([i][0])}catch(e){return e.name} } f()`, "TypeError"},
	{"bigint increment", `function f() { let i=0n,s="abc";try{s.charCodeAt(++i)}catch(e){return e.name+":"+i} } f()`, "TypeError:1"},
	{"bigint argument", `function f() { let i=0n,s="abc";try{s.charCodeAt([i][0])}catch(e){return e.name} } f()`, "TypeError"},
	{"no argument and two", `function f() { let s="abc",i=0;return [s.charCodeAt(),s.charCodeAt(i+1,i++)].join() } f()`, "97,98"},
	{"bound builtin", `function f() { let i=0,s={charCodeAt:String.prototype.charCodeAt.bind("xyz")};let a=s.charCodeAt(++i);return String(a) } f()`, "121"},
	{"string object receiver", `function f() { let i=0,s=new String("abc");return String(s.charCodeAt(++i)) } f()`, "98"},
	{"number receiver", `function f() { let i=0;Number.prototype.charCodeAt=String.prototype.charCodeAt;let a=(12345).charCodeAt(i+1);delete Number.prototype.charCodeAt;return String(a) } f()`, "50"},
	{"object receiver", `function f() { let i=0,s={toString(){return "xyz"},charCodeAt:String.prototype.charCodeAt};return String(s.charCodeAt(++i)) } f()`, "121"},
	{"nullish receiver", `function f() { let i=0,c=String.prototype.charCodeAt;try{c.call(undefined,++i)}catch(e){return e.name+":"+i} } f()`, "TypeError:1"},
	{"proxy receiver", `function f() { let log=[],i=0,p=new Proxy({charCodeAt:String.prototype.charCodeAt,toString(){return "pq"}},{get(t,k){log.push(String(k));return t[k]}});let a=p.charCodeAt(++i);return [a,log.join("/")].join() } f()`, "113,charCodeAt/Symbol(Symbol.toPrimitive)/toString"},
	{"proxy method", `function f() { let i=0,saved=String.prototype.charCodeAt,c=new Proxy(saved,{apply(t,th,args){return Reflect.apply(t,th,args)+1000}}),s=new String("abc");s.charCodeAt=c;let a=s.charCodeAt(++i);String.prototype.charCodeAt=c;let b="abc".charCodeAt(i+1);String.prototype.charCodeAt=saved;return [a,b].join() } f()`, "1098,1099"},
	{"rope and wide receivers", `function f() { let a="ab",b="c€",s=a+b,i=0;return [s.charCodeAt(++i),s.charCodeAt(i+2),("Ā"+a).charCodeAt(i+1)].join() } f()`, "98,8364,98"},
	{"script method frame", `function f(){let i=0,s={n:7,charCodeAt:function(x){let h=0;for(let j=0;j<3;j++)h+=j;return this.n+x+h}};let a=s.charCodeAt(++i);return [a,i].join()} f()`, "11,1"},
	{"script method templates", `function f(){let i=0,h=0,s={};for(let k=0;k<10;k++){s.charCodeAt=(function(n){return function(x){let h=0;for(let j=0;j<3;j++)h+=j;return n+x+h}})(k);h+=s.charCodeAt(++i)}return h} ''+f()`, "130"},
	{"tail position", `function f(s,i) { "use strict"; return s.charCodeAt(i+1) } [f("abc",0),f("abc",1),f("abc",2)].join()`, "98,99,NaN"},
	{"numeric edges", `function f() { let s="a\ud800c",i=-1.5,out=[];out.push(s.charCodeAt(i+1));i=NaN;out.push(s.charCodeAt(i+1));i=Infinity;out.push(s.charCodeAt(i+1));i=-Infinity;out.push(s.charCodeAt(i+1));i=-2;out.push(s.charCodeAt(i+1));i=0;out.push(s.charCodeAt(++i));i=2**53;out.push(s.charCodeAt(i+1));out.push(s.charCodeAt(i-2**53+2.9));return out.join() } f()`, "97,97,NaN,NaN,NaN,55296,NaN,99"},
}

func TestSavedStringCalls(t *testing.T) {
	t.Parallel()
	for _, tc := range savedStringCalls {
		t.Run(tc.name, func(t *testing.T) {
			// Twice in one runtime, the second with the site's cache filled.
			r := New(Config{})
			defer r.Close()
			p := compileForTest(t, tc.source)
			for round := range 2 {
				v, err := r.Run(p)
				if err != nil {
					t.Fatal(err)
				}
				if got := v.String().Go(); got != tc.want {
					t.Fatalf("round %d: got %q, want %q", round, got, tc.want)
				}
			}
		})
	}
}

// stringUnitShow prints a value, a string as its code units in hex, so
// that a lone surrogate prints as itself.
const stringUnitShow = `function show(v){if(typeof v!="string")return String(v);let o="'";for(let k=0;k<v.length;k++)o+=(k?" ":"")+v.charCodeAt(k).toString(16);return o+"'"}
`

// stringUnitMethods are the methods stringUnitCalls call, in the order
// of their answers.
var stringUnitMethods = [3]string{"codePointAt", "at", "charAt"}

// stringUnitCalls are calls of codePointAt, at and charAt, M in each
// source, as method calls (call_method_w), with each method's answer,
// Node's: every position of ASCII, Latin-1, wide and surrogate strings
// and of ropes, positions that are fractions, negative, NaN, infinite or
// of no int64, arguments that are not numbers or convert by a call, a
// receiver that is not a string, overrides, getters, proxies and bound
// methods.
var stringUnitCalls = []struct {
	name, source string
	want         [3]string
}{
	{"ascii", `function f(s){let o=[];for(let i=-4;i<=s.length+1;i++)o.push(show(s.M(i)));return o.join()} f("abc")`, [3]string{"undefined,undefined,undefined,undefined,97,98,99,undefined,undefined", "undefined,'61','62','63','61','62','63',undefined,undefined", "'','','','','61','62','63','',''"}},
	{"positions", `function f(s){let o=[];for(const i of [-0.5,0.9,-1.5,2.99,-2.99,NaN,Infinity,-Infinity,2**31,2**32+1,2**53,-(2**63),2**63,2**64,1e300,-1e300,-0,5e-324])o.push(show(s.M(i)));return o.join()} f("abc")`, [3]string{"97,97,undefined,99,undefined,97,undefined,undefined,undefined,undefined,undefined,undefined,undefined,undefined,undefined,undefined,97,97", "'61','61','63','63','62','61',undefined,undefined,undefined,undefined,undefined,undefined,undefined,undefined,undefined,undefined,'61','61'", "'61','61','','63','','61','','','','','','','','','','','61','61'"}},
	{"updates", `function f(s){let i=-1,o=[];o.push(show(s.M(++i)));o.push(show(s.M(i++)));o.push(show(s.M(i+1)));o.push(show(s.M(i-3)));return o.join()+":"+i} f("abc")`, [3]string{"97,97,99,undefined:1", "'61','61','63','62':1", "'61','61','63','':1"}},
	{"conversions", `function f(s){let o=[];for(const i of ["1","x",undefined,null,true,false,{valueOf(){return 2}},[1],"-1"," 2 ","0x2"])o.push(show(s.M(i)));return o.join()} f("abc")`, [3]string{"98,97,97,97,98,97,99,98,undefined,99,99", "'62','61','61','61','62','61','63','62','63','63','63'", "'62','61','61','61','62','61','63','62','','63','63'"}},
	{"conversion once", `function f(s){let n=0,i={valueOf(){n++;return 1}};let a=s.M(i);let b=s.M([i][0]);return [show(a),show(b),n].join()} f("abc")`, [3]string{"98,98,2", "'62','62',2", "'62','62',2"}},
	{"symbol", `function f(s){let i=Symbol();try{s.M([i][0])}catch(e){return e.name}} f("abc")`, [3]string{"TypeError", "TypeError", "TypeError"}},
	{"bigint", `function f(s){let i=0n;try{s.M(++i)}catch(e){return e.name+":"+i}} f("abc")`, [3]string{"TypeError:1", "TypeError:1", "TypeError:1"}},
	{"no argument and two", `function f(s){let i=0;return [show(s.M()),show(s.M(i+1,i++)),i].join()} f("abc")`, [3]string{"97,98,1", "'61','62',1", "'61','62',1"}},
	{"pairs", `function f(s){let o=[];for(let i=-1;i<=s.length;i++)o.push(show(s.M(i)));return o.join()} f("a\ud83d\ude00b\udbff\udfff\ude00x\ud800\ud800\udc00\ud83d")`, [3]string{"undefined,97,128512,56832,98,1114111,57343,56832,120,55296,65536,56320,55357,undefined", "'d83d','61','d83d','de00','62','dbff','dfff','de00','78','d800','d800','dc00','d83d',undefined", "'','61','d83d','de00','62','dbff','dfff','de00','78','d800','d800','dc00','d83d',''"}},
	{"latin1 and wide", `function f(s){let o=[];for(let i=-s.length-1;i<=s.length;i++)o.push(show(s.M(i)));return o.join()} f("a\u00e9\u20ac\u4e2d\u00ff\u0100\u0080\u007f")`, [3]string{"undefined,undefined,undefined,undefined,undefined,undefined,undefined,undefined,undefined,97,233,8364,20013,255,256,128,127,undefined", "undefined,'61','e9','20ac','4e2d','ff','100','80','7f','61','e9','20ac','4e2d','ff','100','80','7f',undefined", "'','','','','','','','','','61','e9','20ac','4e2d','ff','100','80','7f',''"}},
	{"latin1 string", `function f(s){let o=[];for(let i=0;i<s.length;i++)o.push(show(s.M(i)));return o.join()} f("\u00e9t\u00e9\u00ff\u0080")`, [3]string{"233,116,233,255,128", "'e9','74','e9','ff','80'", "'e9','74','e9','ff','80'"}},
	{"rope", `function f(n){let a="abcdefghijklmnopqrstuvwxyz012345",o=[];for(let j=0;j<n;j++){let s=a+"6789abcdefghijklmnopqrstuvwxyz0123456789\u20ac";o.push(show(s.M(j*9-40)))}return o.join()} f(12)`, [3]string{"undefined,undefined,undefined,undefined,undefined,102,111,120,54,102,111,120", "'37','67','70','79','37','66','6f','78','36','66','6f','78'", "'','','','','','66','6f','78','36','66','6f','78'"}},
	{"wide rope", `function f(n){let a="\u4e2dbcdefghijklmnopqrstuvwxyz012345",o=[];for(let j=0;j<n;j++){let s=a+"6789abcdefghijklmnopqrstuvwxyz0123456789\ud83d\ude00";o.push(show(s.M(j*7-30)))}return o.join()} f(12)`, [3]string{"undefined,undefined,undefined,undefined,undefined,102,109,116,48,55,101,108", "'69','70','77','33','d83d','66','6d','74','30','37','65','6c'", "'','','','','','66','6d','74','30','37','65','6c'"}},
	{"string object receiver", `function f(){let i=0,s=new String("abc");return show(s.M(++i))} f()`, [3]string{"98", "'62'", "'62'"}},
	{"number receiver", `function f(){let i=0;Number.prototype.M=String.prototype.M;let a=(12345).M(i+1);delete Number.prototype.M;return show(a)} f()`, [3]string{"50", "'32'", "'32'"}},
	{"object receiver", `function f(){let i=0,s={toString(){return "xyz"},M:String.prototype.M};return show(s.M(++i))} f()`, [3]string{"121", "'79'", "'79'"}},
	{"nullish receiver", `function f(){let i=0,c=String.prototype.M;try{c.call(undefined,++i)}catch(e){return e.name+":"+i}} f()`, [3]string{"TypeError:1", "TypeError:1", "TypeError:1"}},
	{"proxy receiver", `function f(){let log=[],i=0,p=new Proxy({M:String.prototype.M,toString(){return "pq"}},{get(t,k){log.push(String(k));return t[k]}});let a=p.M(++i);return [show(a),log.join("/")].join()} f()`, [3]string{"113,codePointAt/Symbol(Symbol.toPrimitive)/toString", "'71',at/Symbol(Symbol.toPrimitive)/toString", "'71',charAt/Symbol(Symbol.toPrimitive)/toString"}},
	{"proxy method", `function f(){let i=0,saved=String.prototype.M,c=new Proxy(saved,{apply(t,th,args){return "p"+show(Reflect.apply(t,th,args))}}),s=new String("abc");s.M=c;let a=s.M(++i);String.prototype.M=c;let b="abc".M(i+1);String.prototype.M=saved;return [a,b].join()} f()`, [3]string{"p98,p99", "p'62',p'63'", "p'62',p'63'"}},
	{"bound", `function f(){let i=0,s={M:String.prototype.M.bind("xyz")};return show(s.M(++i))} f()`, [3]string{"121", "'79'", "'79'"}},
	{"override", `function read(i){let v="abc".M(i+0);return show(v)} function many(){let o=[];for(let i=0;i<4;i++)o.push(read(i&1));return o.join()} function f(){let saved=String.prototype.M,out=[many()];String.prototype.M=function(i){return "o"+i};out.push(many());String.prototype.M=saved;out.push(many());return out.join("|")} f()`, [3]string{"97,98,97,98|'6f 30','6f 31','6f 30','6f 31'|97,98,97,98", "'61','62','61','62'|'6f 30','6f 31','6f 30','6f 31'|'61','62','61','62'", "'61','62','61','62'|'6f 30','6f 31','6f 30','6f 31'|'61','62','61','62'"}},
	{"prototype getter", `function f(){let n=0,saved=String.prototype.M,i=0;Object.defineProperty(String.prototype,"M",{configurable:true,get(){n++;return saved}});let a="abc".M(++i),b="abc".M(i+1);Object.defineProperty(String.prototype,"M",{configurable:true,writable:true,value:saved});return [show(a),show(b),n].join()} f()`, [3]string{"98,99,2", "'62','63',2", "'62','63',2"}},
	{"argument replaces method", `function f(){let s="abc",saved=String.prototype.M,i={valueOf(){String.prototype.M=function(){return 7};return 0}},a=s.M(++i);String.prototype.M=saved;return [show(a),i].join()} f()`, [3]string{"98,1", "'62',1", "'62',1"}},
	{"identity", `function f(s){let a=s.M(1),b=s.M(1),c="xbx".M(1);return [a===b,a===c,typeof a].join()} f("abc")`, [3]string{"true,true,number", "true,true,string", "true,true,string"}},
	{"result kept", `function f(s){let o=[];for(let i=0;i<s.length;i++){let c=s.M(i);o.push(c)}return o.map(show).join()} f("ab\u00e9\u20ac")`, [3]string{"97,98,233,8364", "'61','62','e9','20ac'", "'61','62','e9','20ac'"}},
}

// stringUnitMethod is a word M, stringUnitCalls' method.
var stringUnitMethod = regexp.MustCompile(`\bM\b`)

// stringUnitSource is stringUnitCalls' source for method m.
func stringUnitSource(source, m string) string {
	return stringUnitShow + stringUnitMethod.ReplaceAllString(source, m)
}

func TestStringUnitCalls(t *testing.T) {
	t.Parallel()
	for _, tc := range stringUnitCalls {
		for k, m := range stringUnitMethods {
			t.Run(m+"/"+tc.name, func(t *testing.T) {
				// Twice in one runtime, the second with the sites' caches
				// filled.
				r := New(Config{})
				defer r.Close()
				p := compileForTest(t, stringUnitSource(tc.source, m))
				for round := range 2 {
					v, err := r.Run(p)
					if err != nil {
						t.Fatal(err)
					}
					if got := v.String().Go(); got != tc.want[k] {
						t.Fatalf("round %d: got %q, want %q", round, got, tc.want[k])
					}
				}
			})
		}
	}
}
