//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || (darwin && arm64))

package vm

import (
	"context"
	"crypto/md5"
	_ "embed"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

//go:embed testdata/md5.js
var jitMD5Source string

const jitMD5Setup = `var input='a'.repeat(1024);function hashes(){return md5(input)}`

func TestJITStringPacking(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	v, err := r.Run(compileForTest(t, `function pack(s,a){for(let i=0;i<64;i+=4)a[i>>2]=s.charCodeAt(i)+(s.charCodeAt(i+1)<<8)+(s.charCodeAt(i+2)<<16)+(s.charCodeAt(i+3)<<24);return a[0]}let a=new Array(16).fill(0);pack('a'.repeat(64),a)`))
	if err != nil || !v.IsNumber() || v.Number() != 1633771873 || r.jit == nil || r.jit.hosts != 0 {
		if r.jit != nil {
			t.Logf("hosts=%d guards=%d", r.jit.hosts, r.jit.guards)
		}
		t.Fatalf("packing: %v, %v", v, err)
	}
}

// Writes past an array's length grow it in Go, at a host exit; callbacks
// between them see every committed element and none beyond.
func TestJITStringArrayGrowth(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	v, err := r.Run(compileForTest(t, `function pack(s){let a=[];for(let i=0;i<64;i++)a[i]=s.charCodeAt(i);return a}let a=pack('A'.repeat(64));a.length===64&&a.every(n=>n===65)`))
	if err != nil || !v.IsBool() || !v.Truthy() || r.JITStats().SSAEntries == 0 {
		t.Fatalf("growth: %v, %v", v, err)
	}
	for _, tc := range []struct{ name, setup, body, check string }{
		{"dense", ``, `a[i]=s.charCodeAt(i);`, `a.length===64&&a.every((n,i)=>n===65+i)`},
		{"gap", ``, `a[i*3]=s.charCodeAt(i);`, `a.length===190&&Object.keys(a).length===64&&!(1 in a)&&a[189]===128`},
		{"alias callback", `function see(a,i){if(a.length!==i+1||a[i]!==65+i)throw Error('uncommitted')}`, `a[i]=s.charCodeAt(i);see(a,i);`, `a.length===64&&a[63]===128`},
		{"method mutation", `function see(a,i){if(i===3)String.prototype.charCodeAt=function(){return 42}}`, `a[i]=s.charCodeAt(i);see(a,i);`, `a.length===64&&a[3]===68&&a[4]===42`},
		{"inherited setter", `let hits=0;function see(a,i){if(i===3)Object.defineProperty(Array.prototype,'4',{set(v){hits++},configurable:true})}`, `a[i]=s.charCodeAt(i);see(a,i);`, `a.length===64&&hits===1&&!Object.hasOwn(a,4)&&a[5]===70`},
		{"freeze", `function see(a,i){if(i===3)Object.freeze(a)}`, `a[i]=s.charCodeAt(i);see(a,i);`, `good&&a.length===4&&a[3]===68`},
		{"throw", `let hits=0;function see(a,i){if(i===3)String.prototype.charCodeAt=function(){throw new TypeError('stop')}}`, `a[i]=s.charCodeAt(i);see(a,i);`, `good&&a.length===4&&a[3]===68`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, enabled := range []bool{false, true} {
				r := jitRuntimeForTest(t, Config{JIT: enabled})
				source := `'use strict';` + tc.setup + `let a,good=false;function save(v){a=v}function pack(s){let a=[];save(a);for(let i=0;i<64;i++){` + tc.body + `}return a}let s='';for(let i=0;i<64;i++)s+=String.fromCharCode(65+i);try{pack(s)}catch(e){good=e instanceof TypeError};` + tc.check
				v, err := r.Run(compileForTest(t, source))
				if err != nil || !v.IsBool() || !v.Truthy() || enabled && r.JITStats().SSAEntries == 0 {
					t.Fatalf("growth JIT %v: %v, %v", enabled, v, err)
				}
			}
		})
	}
	// Sparse lengths must never be committed as a dense slice bound.
	r = jitRuntimeForTest(t, Config{JIT: true})
	v, err = r.Run(compileForTest(t, `function pack(s,a){let unused=[];for(let i=0;i<2;i++)a[i]=s.charCodeAt(i);return a}let a=[1];a.length=1000000;pack('AB',a);a.length===1000000&&a[0]===65&&a[1]===66`))
	if err != nil || !v.IsBool() || !v.Truthy() {
		t.Fatalf("sparse growth: %v, %v", v, err)
	}
}

func TestJITArrayGrowthCancellation(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	r.SetContext(ctx)
	_, err := r.Run(compileForTest(t, `function pack(s){let a=[];for(let i=0;;i=(i+1)&15)a[i]=s.charCodeAt(i);return a}pack('A'.repeat(16))`))
	if !errors.Is(err, context.DeadlineExceeded) || r.jit == nil || r.jit.budgets == 0 {
		t.Fatalf("bounded growth cancellation: %v", err)
	}
}

func TestJITStringBoundaries(t *testing.T) {
	for _, tc := range []struct{ name, setup, body, check string }{
		{"utf16", `let s='A\u1234\ud83d\ude00\ud800\udc00';s.charCodeAt(0);`, `a[i]=s.charCodeAt(i);`, `a.join(',')==='65,4660,55357,56832,55296,56320'`},
		{"rope", `let s='A'+'\u1234'+'\ud83d'+'\ude00'+'\ud800'+'\udc00';`, `a[i]=s.charCodeAt(i);`, `a.join(',')==='65,4660,55357,56832,55296,56320'`},
		{"indices", `let s='ABCDEF';`, `a[i]=s.charCodeAt(i-0.5);`, `a.join(',')==='65,65,66,67,68,69'`},
		{"out of bounds", `let s='ABCDEF';`, `a[i]=s.charCodeAt(i-1);`, `Number.isNaN(a[0])&&a.slice(1).join(',')==='65,66,67,68,69'`},
		{"replace", `let s='ABCDEF';String.prototype.charCodeAt=function(i){return 100+i};`, `a[i]=s.charCodeAt(i);`, `a.join(',')==='100,101,102,103,104,105'`},
		{"getter", `let s='ABCDEF',hits=0;let original=String.prototype.charCodeAt;Object.defineProperty(String.prototype,'charCodeAt',{get(){hits++;return original}});`, `a[i]=s.charCodeAt(i);`, `hits===6&&a.join(',')==='65,66,67,68,69,70'`},
		{"index coercion", `let s='ABCDEF',hits=0,index={valueOf(){return hits++}};`, `a[i]=s.charCodeAt(index);`, `hits===6&&a.join(',')==='65,66,67,68,69,70'`},
		{"mutate during coercion", `let s='ABCDEF',hits=0,index={valueOf(){hits++;String.prototype.charCodeAt=function(){return 99};return 0}};`, `a[i]=s.charCodeAt(index);`, `hits===1&&a.join(',')==='65,99,99,99,99,99'`},
		{"callback mutation", `let s='ABCDEF';function cb(i){if(i===2)String.prototype.charCodeAt=function(){return 88}}`, `a[i]=s.charCodeAt(i);cb(i);`, `a.join(',')==='65,66,67,88,88,88'`},
		{"custom method", `let s={charCodeAt(i){return 10+i}};`, `a[i]=s.charCodeAt(i);`, `a.join(',')==='10,11,12,13,14,15'`},
		{"throw", `let s='ABCDEF',hits=0,index={valueOf(){if(++hits===3)throw new TypeError('stop');return hits-1}};`, `a[i]=s.charCodeAt(index);`, `good&&hits===3&&a.join(',')==='65,66,0,0,0,0'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, enabled := range []bool{false, true} {
				r := jitRuntimeForTest(t, Config{JIT: enabled})
				source := tc.setup + `let a=new Array(6).fill(0),good=false;function f(s,a){for(let i=0;i<6;i++){` + tc.body + `}return a[0]}try{f(s,a)}catch(e){good=e instanceof TypeError&&e.message==='stop'};` + tc.check
				v, err := r.Run(compileForTest(t, source))
				if err != nil || !v.IsBool() || !v.Truthy() || enabled && r.JITStats().SSAEntries == 0 {
					t.Fatalf("JIT %v: %v, %v", enabled, v, err)
				}
			}
		})
	}
}

func TestJITArrayLiteralBoundary(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		r := jitRuntimeForTest(t, Config{JIT: enabled})
		v, err := r.Run(compileForTest(t, `let original={x:7};function f(o){let a=[o,1,2];for(let i=1;i<3;i++)a[i]+=1;return a}let a=f(original);a[0]===original&&a[1]===2&&a[2]===3&&a.length===3`))
		if err != nil || !v.IsBool() || !v.Truthy() || enabled && r.JITStats().SSAEntries == 0 {
			t.Fatalf("array literal JIT %v: %v, %v", enabled, v, err)
		}
	}
}

func TestJITStringCalleePermissions(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	v, err := r.Run(compileForTest(t, `function pack(s,a,method){for(let i=0;i<64;i++)a[i]=s.charCodeAt(i);return a[0]}function outer(s,a,method){let sum=0;for(let j=0;j<4;j++)sum+=pack(s,a,method);return sum}let a=new Array(64).fill(0);outer('a'.repeat(64),a,String.prototype.charCodeAt)===388`))
	if err != nil || !v.IsBool() || !v.Truthy() || r.JITStats().SSAEntries == 0 {
		t.Fatalf("string callee permissions: %v, %v", v, err)
	}
}

func TestJITStringRealms(t *testing.T) {
	r := jitRuntimeForTest(t, Config{JIT: true})
	other := r.NewRealm()
	setup := compileForTest(t, `function pack(s,a){for(let i=0;i<6;i++)a[i]=s.charCodeAt(i);return a[0]}var a=new Array(6).fill(0);true`)
	call := compileForTest(t, `pack('ABCDEF',a)===65&&a[5]===70`)
	for _, realm := range []*Realm{r.Realm, other, r.Realm, other} {
		if _, err := r.RunIn(realm, setup); err != nil {
			t.Fatal(err)
		}
		v, err := r.RunIn(realm, call)
		if err != nil || !v.IsBool() || !v.Truthy() || r.jit == nil || r.jit.hosts != 0 {
			t.Fatalf("realm character read: %v, %v", v, err)
		}
	}
}

func TestJITMD5(t *testing.T) {
	inputs := []string{
		"", "a", "abc", "message digest", "abcdefghijklmnopqrstuvwxyz",
		"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789",
		strings.Repeat("1234567890", 8),
		strings.Repeat("\x00\x80\xff", 100),
	}
	for _, n := range []int{55, 56, 63, 64, 65, 119, 120, 127, 128, 129, 1024} {
		inputs = append(inputs, strings.Repeat("a", n))
	}
	for _, mode := range []string{"existing", "native", "marking"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "marking" {
				jitMarkingForTest(t)
			}
			r := jitRuntimeForTest(t, Config{JIT: mode != "existing"})
			if _, err := r.Run(compileForTest(t, jitMD5Source)); err != nil {
				t.Fatal(err)
			}
			for _, input := range inputs {
				want := fmt.Sprintf("%x", md5.Sum([]byte(input)))
				v, err := r.Run(compileForTest(t, "md5("+strconv.Quote(input)+")"))
				if err != nil || !v.IsString() || v.String().Go() != want {
					t.Fatalf("MD5 of %d bytes: %v, %v; want %s", len(input), v, err, want)
				}
			}
			if mode != "existing" && r.JITStats().SSAEntries == 0 {
				t.Fatal("MD5 did not run native code")
			}
		})
	}
}

// Validate every complete hash, including the compilation its first use triggers.
func BenchmarkJITMD5FirstUse(b *testing.B) {
	setup := compileForTest(b, jitMD5Source+jitMD5Setup)
	call := compileForTest(b, `hashes()`)
	for _, mode := range []string{"existing", "native"} {
		b.Run(mode, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			b.StopTimer()
			for range b.N {
				r := New(Config{JIT: mode == "native"})
				if _, err := r.Run(setup); err != nil {
					b.Fatal(err)
				}
				// Construction, parsing and declarations are excluded. Native
				// compilation triggered by this first complete hash is included.
				b.StartTimer()
				v, err := r.Run(call)
				b.StopTimer()
				if err != nil || !v.IsString() || v.String().Go() != "c9a34cfc85d982698c6ac89f76071abd" || mode == "native" && (r.jit == nil || r.jit.entries == 0) {
					b.Fatalf("first MD5: %v, %v", v, err)
				}
				r.Close()
				r.ReleaseClosed()
			}
		})
	}
}

func BenchmarkJITMD5(b *testing.B) {
	setup := compileForTest(b, jitMD5Source+jitMD5Setup)
	call := compileForTest(b, `hashes()`)
	for _, mode := range []string{"interpreter", "existing", "native"} {
		b.Run(mode, func(b *testing.B) {
			previous := treeTier.Swap(mode != "interpreter")
			defer treeTier.Store(previous)
			r := New(Config{JIT: mode == "native"})
			defer func() { r.Close(); r.ReleaseClosed() }()
			if _, err := r.Run(setup); err != nil {
				b.Fatal(err)
			}
			run := func() {
				v, err := r.Run(call)
				if err != nil || !v.IsString() || v.String().Go() != "c9a34cfc85d982698c6ac89f76071abd" {
					b.Fatalf("MD5: %v, %v", v, err)
				}
			}
			for range 100 {
				run()
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				run()
			}
			b.StopTimer()
			if mode == "native" {
				packed := false
				if r.jit != nil {
					for key, entry := range r.jit.cache {
						if fn := key.Value(); fn != nil && fn.Name == "md5blk" {
							packed = entry.ssa != nil && !entry.entrySlow
						}
					}
				}
				if !packed {
					b.Fatal("block packing did not retain native execution")
				}
			}
			if r.jit != nil {
				b.ReportMetric(float64(r.jitCodeBytes()), "code-B")
				b.ReportMetric(float64(r.jit.hosts)/float64(b.N+100), "hosts/hash")
				b.ReportMetric(float64(r.jit.guards)/float64(b.N+100), "guards/hash")
				for key, entry := range r.jit.cache {
					fn := key.Value()
					if fn != nil {
						b.Logf("%s: code=%v slow=%v hosts=%d guards=%d", fn.Name, entry.ssa != nil, entry.entrySlow, entry.ssaStats.hosts, entry.ssaStats.guards)
					}
				}
			}
		})
	}
}
