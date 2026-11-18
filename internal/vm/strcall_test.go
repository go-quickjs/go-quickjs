package vm

import "testing"

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
