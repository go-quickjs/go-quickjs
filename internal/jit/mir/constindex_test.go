package mir

import (
	"bytes"
	"encoding/binary"
	"testing"

	jscompile "github.com/go-quickjs/go-quickjs/internal/compiler"
	"github.com/go-quickjs/go-quickjs/internal/jit/abi"
	jitcompile "github.com/go-quickjs/go-quickjs/internal/jit/compile"
	"github.com/go-quickjs/go-quickjs/internal/jit/ssa"
	"github.com/go-quickjs/go-quickjs/internal/parser"
)

// conversionsForTest counts the instructions of arch's code that convert
// a double to a 64-bit integer, as an element's key is converted to its
// index: amd64's CVTTSD2SI of a 64-bit register (F2, a REX.W prefix, 0F
// 2C), arm64's FCVTZS of a double to a 64-bit register.
func conversionsForTest(arch string, code []byte) int {
	n := 0
	if arch == "amd64" {
		for i := 0; i+4 <= len(code); i++ {
			if code[i] == 0xf2 && code[i+1]&0xf8 == 0x48 && code[i+2] == 0x0f && code[i+3] == 0x2c {
				n++
			}
		}
		return n
	}
	for i := 0; i+4 <= len(code); i += 4 {
		if binary.LittleEndian.Uint32(code[i:])&0xfffffc00 == 0x9e780000 {
			n++
		}
	}
	return n
}

// constIndexCodeForTest compiles the source's one function for arch.
func constIndexCodeForTest(t *testing.T, arch, src string) []byte {
	t.Helper()
	a, err := parser.Parse(src, parser.Options{})
	if err != nil {
		t.Fatal(err)
	}
	fn, err := jscompile.Compile(a, jscompile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	fn = fn.Constants[0].Fn
	p, err := jitcompile.LowerReg(fn.RegCode(), fn)
	if err != nil {
		t.Fatal(err)
	}
	f, err := ssa.Build(p)
	if err != nil {
		t.Fatal(err)
	}
	ssa.Optimize(f)
	enc := abi.Encoding{ValueSize: 16, StoreRef: 1, CallGo: 1}
	var code *Code
	if arch == "amd64" {
		code, err = CompileAMD64(f, enc)
	} else {
		code, err = CompileARM64(f, enc)
	}
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Clone(code.Bytes[:code.Text])
}

// An element's key that is a constant integer is its index as it is, on
// both backends (constIndex), where a key the code does not know is
// converted and checked at each access: the same reads and writes by such
// keys have one conversion more an access. (What else of a write converts
// a key -- the appending routine's, a typed array's store's -- is the
// same in both.)
func TestConstantElementKeysConvertNothing(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			for _, c := range []struct {
				constant, variable string
				accesses           int
			}{
				{"function f(a){return a[3]}", "function f(a,i){return a[i]}", 1},
				{"function f(a){a[3]=1;return a}", "function f(a,i){a[i]=1;return a}", 1},
				{"function f(a){a[3]=a[5];a[7]=a[1];return a}", "function f(a,i,j,k,l){a[i]=a[j];a[k]=a[l];return a}", 4},
			} {
				k := conversionsForTest(arch, constIndexCodeForTest(t, arch, c.constant))
				v := conversionsForTest(arch, constIndexCodeForTest(t, arch, c.variable))
				if v-k != c.accesses {
					t.Errorf("%s: %d conversions of a key, %d with keys the code does not know; want %d fewer, one an access", c.constant, k, v, c.accesses)
				}
			}
			if n := conversionsForTest(arch, constIndexCodeForTest(t, arch, "function f(a){return a[3]}")); n != 0 {
				t.Errorf("a[3]'s read converts %d keys, want 0", n)
			}
		})
	}
}
