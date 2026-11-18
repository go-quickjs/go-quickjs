//go:build quickjs_jit && !android && !ios && ((linux && amd64) || (windows && amd64) || ((darwin || linux) && arm64))

package vm

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/go-quickjs/go-quickjs/internal/jit/mir"
)

// regTrySrc's loops run a try statement at every turn: one that never
// throws, one that throws and catches at every turn, a finally clause,
// and a catch some turns reach, each carrying an int32 sum.
const regTrySrc = `
var res
function caught(n) { var i, s = 0; for (i = 0; i < n; i++) { try { s = (s + i) | 0 } catch (e) { s = 0 } } res = s; return n }
function thrown(n) { var i, s = 0; for (i = 0; i < n; i++) { try { throw i } catch (e) { s = (s + e) | 0 } } res = s; return n }
function finals(n) { var i, s = 0; for (i = 0; i < n; i++) { try { s = (s + i) | 0 } finally { s ^= 1 } } res = s; return n }
function some(n) {
	var i, s = 0
	for (i = 0; i < n; i++) {
		try { if (i % 97 === 3) throw new RangeError("r" + i); s = (s * 3 + i) | 0 } catch (e) { s = (s + e.message.length) | 0 }
	}
	res = s
	return n
}
`

// Register code's loops of try statements answer as the interpreter
// does, a throw at every turn and some turns' caught natively at their
// handler, with the collector idle and marking. On amd64 each conversion
// of an int32 to a double (ssa.OpI32ToF64) zeroes its register first
// (mir's intToF64): cvtsi2sd writes the register's low half alone, and
// the conversions of a function, all to the same scratch register, each
// waited for the last -- a register loop's conversion of its sum for the
// value its exit gives, made at every turn, held up the next turn's
// conversion that its addition waits for (try_catch, try_throw and
// try_finally 1.1 to 1.2 times the stack form's code).
func TestJITRegTryLoops(t *testing.T) {
	if !jitSSABackend {
		t.Skip("no SSA backend on this architecture")
	}
	for _, marking := range []bool{false, true} {
		t.Run(fmt.Sprintf("marking=%v", marking), func(t *testing.T) {
			if marking {
				jitMarkingForTest(t)
			}
			codes := map[string]*mir.Code{}
			mir.Annotate = true
			JITCodeHook = func(f JITFunction, _ []byte, c *mir.Code) { codes[f.Name] = c }
			defer func() { JITCodeHook, mir.Annotate = nil, false }()
			rts := regNativeRuntimes(t, regTrySrc)
			rts[0].jitStress = jitStressConfig{}
			var got [2]string
			for k, r := range rts {
				for _, name := range []string{"caught", "thrown", "finals", "some"} {
					fn, _ := r.GetProp(Obj(r.Global()), r.Intern(name))
					for range 3 {
						v, err := r.Call(fn, Undefined, []Value{Float(30000)})
						res, _ := r.GetProp(Obj(r.Global()), r.Intern("res"))
						got[k] += regNativeShow(r, v, err) + ":" + regNativeShow(r, res, nil) + ";"
					}
				}
			}
			const want = "30000:449985000;30000:449985000;30000:449985000;" + // Node's
				"30000:449985000;30000:449985000;30000:449985000;" +
				"30000:449985000;30000:449985000;30000:449985000;" +
				"30000:1454474640;30000:1454474640;30000:1454474640;"
			if got[0] != got[1] || got[0] != want {
				t.Errorf("with the JIT %q, without %q, want %q", got[0], got[1], want)
			}
			if rts[0].RegNativeStats().Entries == 0 {
				t.Error("no native code entered")
			}
			if runtime.GOARCH != "amd64" {
				return
			}
			for _, name := range []string{"caught", "thrown", "finals"} {
				c := codes[name]
				if c == nil {
					t.Errorf("%s was not compiled", name)
					continue
				}
				conversions := 0
				for _, m := range c.Marks {
					if strings.HasSuffix(m.Tag, " i2f") {
						conversions++
					}
				}
				// Each a cvtsi2sd -- F2, a REX prefix where a register is
				// past the eighth, 0F 2A -- right after an xorpd of its
				// register -- 66, a REX prefix, 0F 57.
				zeroed := 0
				text := c.Bytes[:c.Text]
				for k := 5; k+3 < len(text); k++ {
					b, x := text[k:], text[:k]
					if b[0] != 0xF2 || !(b[1] == 0x0F && b[2] == 0x2A || b[1]&0xF0 == 0x40 && b[2] == 0x0F && b[3] == 0x2A) {
						continue
					}
					if x[k-4] == 0x66 && x[k-3] == 0x0F && x[k-2] == 0x57 || x[k-5] == 0x66 && x[k-4]&0xF0 == 0x40 && x[k-3] == 0x0F && x[k-2] == 0x57 {
						zeroed++
					}
				}
				if conversions == 0 || zeroed < conversions {
					t.Errorf("%s: %d conversions of an int32 to a double, %d of them after an xorpd", name, conversions, zeroed)
				}
			}
		})
	}
}
