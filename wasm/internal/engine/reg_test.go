package engine

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-quickjs/go-quickjs/wasm/internal/decoder"
	"github.com/go-quickjs/go-quickjs/wasm/wat"
)

// instantiateReg instantiates the module of src, its bodies compiled to
// register code and run by the interpreter alone, whatever WASM_JIT asks.
func instantiateReg(t *testing.T, src string) *Instance {
	t.Helper()
	b, err := wat.Compile([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	m, derr := decoder.Decode(b, decoder.CoreLimits())
	if derr != nil {
		t.Fatal(derr)
	}
	if derr := decoder.ValidateFunctions(m, decoder.CoreLimits()); derr != nil {
		t.Fatal(derr)
	}
	code := testCode(t, &Code{Module: m, fns: make([]atomic.Pointer[compiled], len(m.Functions))})
	inst, err := Instantiate(context.Background(), code, nil)
	if err != nil {
		t.Fatal(err)
	}
	return inst
}

// regOf returns the register code export name's function runs, nil if it
// runs the tree or has not been compiled.
func regOf(inst *Instance, name string) *regCode {
	ext, _ := inst.Export(name)
	p := inst.Code.fns[ext.Func.Index].Load()
	if p == nil {
		return nil
	}
	return p.reg
}

const regKernels = `(module
  (memory 1)
  (global $g (mut i32) (i32.const 7))
  (global $h (mut i64) (i64.const -2))
  (global $d (mut f64) (f64.const 0.5))
  (func (export "add") (param i32 i32) (result i32) (i32.add (local.get 0) (local.get 1)))
  (func (export "subimm") (param i32) (result i32) (i32.sub (local.get 0) (i32.const -3)))
  (func (export "rsub") (param i32) (result i32) (i32.sub (i32.const 10) (local.get 0)))
  (func (export "shifts") (param i32) (result i32)
    (i32.xor (i32.shl (local.get 0) (i32.const 35)) (i32.shr_s (local.get 0) (i32.const 1))))
  (func (export "rot") (param i32 i32) (result i32) (i32.rotr (local.get 0) (local.get 1)))
  (func (export "big") (param i32) (result i32) (i32.mul (local.get 0) (i32.const 0x9E3779B1)))
  (func (export "i64mix") (param i64 i64) (result i64)
    (i64.add (i64.mul (local.get 0) (i64.const 0x123456789)) (i64.rotl (local.get 1) (i64.const 7))))
  (func (export "i64neg") (param i64) (result i64) (i64.add (local.get 0) (i64.const -5)))
  (func (export "f32") (param f32 f32) (result f32)
    (f32.sqrt (f32.add (f32.mul (local.get 0) (local.get 1)) (f32.const 1.5))))
  (func (export "f64") (param f64) (result f64)
    (f64.copysign (f64.floor (f64.div (local.get 0) (f64.const 3))) (f64.const -0)))
  (func (export "min") (param f64 f64) (result f64) (f64.min (local.get 0) (local.get 1)))
  (func (export "conv") (param f64) (result i64) (i64.trunc_f64_s (local.get 0)))
  (func (export "sat") (param f32) (result i32) (i32.trunc_sat_f32_u (local.get 0)))
  (func (export "wrap") (param i64) (result i32) (i32.wrap_i64 (local.get 0)))
  (func (export "ext") (param i32) (result i64) (i64.extend_i32_s (local.get 0)))
  (func (export "zext") (param i32) (result i64) (i64.extend_i32_u (local.get 0)))
  (func (export "bits") (param f32) (result i32) (i32.reinterpret_f32 (local.get 0)))
  (func (export "cmp") (param i32 i32) (result i32)
    (i32.add (i32.lt_s (local.get 0) (local.get 1)) (i32.mul (i32.lt_u (local.get 0) (local.get 1)) (i32.const 2))))
  (func (export "fcmp") (param f64 f64) (result i32) (f64.lt (local.get 0) (local.get 1)))
  (func (export "eqz") (param i64) (result i32) (i64.eqz (local.get 0)))
  (func (export "pop") (param i32) (result i32) (i32.popcnt (local.get 0)))
  (func (export "div") (param i32 i32) (result i32) (i32.div_s (local.get 0) (local.get 1)))
  (func (export "rem") (param i64 i64) (result i64) (i64.rem_u (local.get 0) (local.get 1)))
  (func (export "sel") (param i32 i32 i32) (result i32) (select (local.get 0) (local.get 1) (local.get 2)))
  (func (export "globals") (result i64)
    (global.set $g (i32.add (global.get $g) (i32.const 1)))
    (global.set $h (i64.mul (global.get $h) (i64.const 3)))
    (global.set $d (f64.add (global.get $d) (f64.const 0.25)))
    (i64.add (i64.add (i64.extend_i32_u (global.get $g)) (global.get $h)) (i64.trunc_f64_s (f64.mul (global.get $d) (f64.const 8)))))
  (func (export "mem") (param i32 i32) (result i64)
    (i32.store offset=3 (local.get 0) (local.get 1))
    (i32.store8 (i32.const 100) (i32.const 0x1ff))
    (i64.store16 offset=200 (i32.const 0) (i64.const -1))
    (f64.store (i32.const 300) (f64.const 2.5))
    (i64.add (i64.add (i64.load32_u offset=3 (local.get 0)) (i64.load8_s (i32.const 100)))
      (i64.add (i64.load16_s (i32.const 200)) (i64.trunc_f64_s (f64.load (i32.const 300))))))
  (func (export "grow") (param i32) (result i32)
    (i32.add (memory.grow (local.get 0)) (i32.mul (memory.size) (i32.const 1000))))
  (func (export "load") (param i32) (result i32) (i32.load (local.get 0)))
  (func (export "trap") (unreachable))
  (func (export "loop") (param $n i32) (result i32)
    (local $i i32) (local $acc i32)
    (block $done
      (loop $l
        (br_if $done (i32.ge_u (local.get $i) (local.get $n)))
        (local.set $acc (i32.add (i32.mul (local.get $acc) (i32.const 33)) (local.get $i)))
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $l)))
    (local.get $acc))
  (func (export "dowhile") (param $n i32) (result i64)
    (local $i i32) (local $s i64)
    (loop $l
      (local.set $s (i64.add (local.get $s) (i64.extend_i32_u (local.get $i))))
      (local.tee $i (i32.add (local.get $i) (i32.const 1)))
      (br_if $l (i32.lt_s (local.get $n))))
    (local.get $s))
  (func (export "switch") (param i32) (result i32)
    (block $c2 (result i32)
      (block $c1 (result i32)
        (block $c0 (result i32)
          (br_table $c0 $c1 $c2 (i32.const 10) (local.get 0)))
        (i32.add (i32.const 1)))
      (i32.add (i32.const 2))))
  (func (export "loopsw") (param $n i32) (result i32)
    (local $i i32) (local $a i32)
    (loop $l
      (block $b
        (br_table $l $b (i32.and (local.get $i) (i32.const 1))
          (local.set $i (i32.add (local.get $i) (i32.const 1)))
          (local.set $a (i32.add (local.get $a) (i32.const 3)))))
      (local.set $a (i32.add (local.get $a) (i32.const 1)))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br_if $l (i32.lt_u (local.get $i) (local.get $n))))
    (local.get $a))
  (func (export "ifelse") (param i32) (result i32 i32)
    (i32.const 5)
    (if (param i32) (result i32 i32) (local.get 0)
      (then (i32.const 1))
      (else (i32.const 2) (i32.add) (i32.const 3))))
  (func (export "swap") (param i32 i32) (result i32 i32) (local.get 1) (local.get 0))
  (func (export "brval") (param i32) (result i32)
    (block (result i32)
      (i32.const 9)
      (br_if 0 (local.get 0))
      (drop)
      (i32.const 4)))
  (func (export "earlyret") (param i32) (result i32)
    (drop (br_if 0 (i32.const 11) (i32.eqz (local.get 0))))
    (i32.const 22))
  (func (export "tee") (param i32) (result i32) (local i32)
    (i32.add (local.tee 1 (i32.mul (local.get 0) (i32.const 3))) (local.get 1)))
  (func (export "readbefore") (param i32) (result i32)
    (i32.sub (local.get 0) (local.set 0 (i32.const 100)) (local.get 0)))
  (func $fib (export "fib") (param i32) (result i64)
    (if (result i64) (i32.lt_u (local.get 0) (i32.const 2))
      (then (i64.extend_i32_u (local.get 0)))
      (else (i64.add (call $fib (i32.sub (local.get 0) (i32.const 1)))
                     (call $fib (i32.sub (local.get 0) (i32.const 2)))))))
  (func $pair (param i32) (result i32 i64) (local.get 0) (i64.extend_i32_s (local.get 0)))
  (func (export "pair") (param i32) (result i64) (local i64)
    (call $pair (local.get 0))
    (local.set 1)
    (i64.extend_i32_u)
    (local.get 1)
    (i64.add))
  (func $count (param i32 i64) (result i64)
    (if (result i64) (i32.eqz (local.get 0)) (then (local.get 1))
      (else (return_call $count (i32.sub (local.get 0) (i32.const 1)) (i64.add (local.get 1) (i64.const 2))))))
  (func (export "tail") (param i32) (result i64) (return_call $count (local.get 0) (i64.const 0)))
)`

// TestRegKernels checks register code's results, exact, against the
// values they must be and, natively and on the Go table, against the
// golden answers.
func TestRegKernels(t *testing.T) {
	reg := instantiateReg(t, regKernels)
	f32 := func(f float32) uint64 { return uint64(math.Float32bits(f)) }
	f64 := func(f float64) uint64 { return math.Float64bits(f) }
	cases := []struct {
		name string
		args []uint64
		want []uint64
	}{
		{"add", []uint64{0xffffffff, 2}, []uint64{1}},
		{"subimm", []uint64{1}, []uint64{4}},
		{"rsub", []uint64{3}, []uint64{7}},
		{"shifts", []uint64{0x80000003}, []uint64{0x18 ^ 0xc0000001}},
		{"rot", []uint64{1, 1}, []uint64{0x80000000}},
		{"big", []uint64{3}, []uint64{(3 * 0x9E3779B1) & 0xffffffff}},
		{"i64mix", []uint64{2, 1 << 60}, []uint64{2*0x123456789 + 8}},
		{"i64neg", []uint64{3}, []uint64{math.MaxUint64 - 1}},
		{"f32", []uint64{f32(2), f32(3.5)}, []uint64{f32(float32(math.Sqrt(8.5)))}},
		{"f64", []uint64{f64(10)}, []uint64{f64(-3)}},
		{"min", []uint64{f64(0), f64(math.Copysign(0, -1))}, []uint64{f64(math.Copysign(0, -1))}},
		{"conv", []uint64{f64(-7.9)}, []uint64{math.MaxUint64 - 6}},
		{"sat", []uint64{f32(-3)}, []uint64{0}},
		{"wrap", []uint64{0x1234567890}, []uint64{0x34567890}},
		{"ext", []uint64{0xfffffffe}, []uint64{math.MaxUint64 - 1}},
		{"zext", []uint64{0xfffffffe}, []uint64{0xfffffffe}},
		{"bits", []uint64{f32(1)}, []uint64{0x3f800000}},
		{"cmp", []uint64{0xffffffff, 1}, []uint64{1}},
		{"fcmp", []uint64{f64(math.NaN()), f64(1)}, []uint64{0}},
		{"eqz", []uint64{1 << 40}, []uint64{0}},
		{"pop", []uint64{0xf0f0}, []uint64{8}},
		{"div", []uint64{uint64(uint32(-7 & 0xffffffff)), 2}, []uint64{uint64(uint32(0xfffffffd))}},
		{"rem", []uint64{17, 5}, []uint64{2}},
		{"sel", []uint64{1, 2, 0}, []uint64{2}},
		{"sel", []uint64{1, 2, 7}, []uint64{1}},
		{"globals", nil, []uint64{8 - 6 + 6}},
		{"mem", []uint64{8, 0xfffffff0}, []uint64{0xfffffff0 - 1 - 1 + 2}},
		{"grow", []uint64{2}, []uint64{1 + 3000}},
		{"loop", []uint64{10}, []uint64{0}},
		{"dowhile", []uint64{100}, []uint64{4950}},
		{"switch", []uint64{0}, []uint64{13}},
		{"switch", []uint64{1}, []uint64{12}},
		{"switch", []uint64{99}, []uint64{10}},
		{"loopsw", []uint64{10}, []uint64{0}},
		{"ifelse", []uint64{1}, []uint64{5, 1}},
		{"ifelse", []uint64{0}, []uint64{7, 3}},
		{"swap", []uint64{1, 2}, []uint64{2, 1}},
		{"brval", []uint64{1}, []uint64{9}},
		{"brval", []uint64{0}, []uint64{4}},
		{"earlyret", []uint64{0}, []uint64{11}},
		{"earlyret", []uint64{5}, []uint64{22}},
		{"tee", []uint64{5}, []uint64{30}},
		{"readbefore", []uint64{1}, []uint64{0xffffff9d}},
		{"fib", []uint64{20}, []uint64{6765}},
		{"pair", []uint64{0xffffffff}, []uint64{0xfffffffe}},
		{"tail", []uint64{100000}, []uint64{200000}},
	}
	// The loop's value: acc = acc*33 + i for i below 10, from 0.
	var acc uint32
	for i := uint32(0); i < 10; i++ {
		acc = acc*33 + i
	}
	for _, c := range cases {
		if c.name == "loop" {
			c.want[0] = uint64(acc)
		}
		got, err := call(t, reg, c.name, c.args...)
		if err != nil {
			t.Fatalf("%s%v: %v", c.name, c.args, err)
		}
		if regOf(reg, c.name) == nil {
			t.Errorf("%s runs no register code", c.name)
		}
		if c.name == "loopsw" {
			continue
		}
		for i, w := range c.want {
			if got[i].N != w {
				t.Errorf("%s%v result %d = %#x, want %#x", c.name, c.args, i, got[i].N, w)
			}
		}
	}
	calls := make([]regCall, len(cases))
	for i, c := range cases {
		calls[i].name = c.name
		for _, a := range c.args {
			calls[i].args = append(calls[i].args, Val{N: a})
		}
	}
	regMatchesGolden(t, regKernels, calls)
}

// TestRegTraps checks the traps register code raises: their messages, and
// their innermost frame's offset, the trapping instruction's, as every case
// of TestTrapOffsets has it where the register compiler takes the function.
func TestRegTraps(t *testing.T) {
	inst := instantiateReg(t, regKernels)
	for _, c := range []struct {
		name string
		args []uint64
		msg  string
	}{
		{"div", []uint64{1, 0}, "divide by zero"},
		{"div", []uint64{0x80000000, 0xffffffff}, "divide result unrepresentable"},
		{"rem", []uint64{1, 0}, "remainder by zero"},
		{"conv", []uint64{math.Float64bits(math.NaN())}, "float unrepresentable in integer range"},
		{"load", []uint64{65533}, "memory access out of bounds"},
		{"trap", nil, "unreachable"},
	} {
		_, err := call(t, inst, c.name, c.args...)
		var tr *Trap
		if !errors.As(err, &tr) || tr.Error() != c.msg {
			t.Fatalf("%s%v = %v, want the trap %q", c.name, c.args, err, c.msg)
		}
	}
	ran := 0
	for _, c := range trapCases {
		inst := instantiateReg(t, c.src)
		ext, _ := inst.Export("f")
		_, err := call(t, inst, "f")
		if regOf(inst, "f") == nil {
			continue
		}
		ran++
		var tr *Trap
		if !errors.As(err, &tr) || tr.Code != c.code || len(tr.Frames) != 1 {
			t.Fatalf("%s: f() = %v, want the trap %q in f's frame", c.name, err, c.code)
		}
		if fr := tr.Frames[0]; fr.Func != ext.Func.Index || fr.Offset != c.offset {
			t.Fatalf("%s: innermost frame: function %d at %#x, want %d at %#x", c.name, fr.Func, fr.Offset, ext.Func.Index, c.offset)
		}
	}
	t.Logf("%d of the trap cases ran on register code", ran)
	if ran < 10 {
		t.Fatalf("%d of the trap cases ran on register code", ran)
	}
}

// TestRegTrapUnderCall checks a trap's frames below a call in register code:
// the trapping function at its instruction, its caller at its call.
func TestRegTrapUnderCall(t *testing.T) {
	inst := instantiateReg(t, `(module
		(func $id (param i32) (result i32) (local.get 0))
		(func $g (param i32) (result i32)
			(i32.div_u (call $id (i32.const 1)) (local.get 0)))
		(func (export "f") (param i32) (result i32) nop (call $g (local.get 0))))`)
	_, err := call(t, inst, "f", 0)
	var tr *Trap
	if !errors.As(err, &tr) || tr.Code != TrapDivByZero {
		t.Fatalf("f(0) = %v, want divide by zero", err)
	}
	want := []Frame{{Module: inst, Func: 1, Offset: 0x2d}, {Module: inst, Func: 2, Offset: 0x34}}
	if len(tr.Frames) != 2 || tr.Frames[0] != want[0] || tr.Frames[1] != want[1] {
		t.Fatalf("frames %+v, want %+v", tr.Frames, want)
	}
	if regOf(inst, "f") == nil {
		t.Fatal("f runs no register code")
	}
}

// TestRegCopyElision pins what the register compiler makes of a loop: the
// locals read in place, the results written to the locals they are set to,
// the constant an immediate and the comparison fused with its branch: five
// instructions a turn for the loop's fifteen.
func TestRegCopyElision(t *testing.T) {
	inst := instantiateReg(t, regKernels)
	if _, err := call(t, inst, "loop", 3); err != nil {
		t.Fatal(err)
	}
	rc := regOf(inst, "loop")
	var ops []regOp
	for _, w := range rc.code {
		ops = append(ops, uint8(w))
	}
	want := []regOp{rBrI32GeU, rI32MulI, rI32Add, rI32AddI, rJmp, rRet1}
	if len(ops) != len(want) {
		t.Fatalf("ops %v, want %v", ops, want)
	}
	for i := range want {
		if ops[i] != want[i] {
			t.Fatalf("ops %v, want %v", ops, want)
		}
	}
	// add writes acc's slot, add_imm i's.
	if a := uint16(rc.code[2] >> 16); a != 2 {
		t.Errorf("i32.add writes slot %d, want local 2's", a)
	}
	if a := uint16(rc.code[3] >> 16); a != 1 {
		t.Errorf("i32.add of the counter writes slot %d, want local 1's", a)
	}
	// The do-while loop's tee is the add's own destination, and the
	// comparison its branch back.
	if _, err := call(t, inst, "dowhile", 3); err != nil {
		t.Fatal(err)
	}
	rc = regOf(inst, "dowhile")
	ops = ops[:0]
	for _, w := range rc.code {
		ops = append(ops, uint8(w))
	}
	want = []regOp{rI64Add, rI32AddI, rBrI32LtS, rRet1}
	if len(ops) != len(want) {
		t.Fatalf("dowhile ops %v, want %v", ops, want)
	}
	for i := range want {
		if ops[i] != want[i] {
			t.Fatalf("dowhile ops %v, want %v", ops, want)
		}
	}
}

// TestRegFarBranches checks a body whose compare-and-branch cannot reach its
// target in 16 bits: it is compiled with 32-bit branches, and runs.
func TestRegFarBranches(t *testing.T) {
	var b strings.Builder
	b.WriteString(`(module (func (export "f") (param i32) (result i32) (local i32)
		(block $out (br_if $out (i32.lt_s (local.get 0) (i32.const 5)))`)
	for i := 0; i < 40000; i++ {
		b.WriteString(` (local.set 1 (i32.add (local.get 1) (i32.const 1)))`)
	}
	b.WriteString(`) (local.get 1)))`)
	inst := instantiateReg(t, b.String())
	for _, c := range [][2]uint64{{1, 0}, {9, 40000}} {
		got, err := call(t, inst, "f", c[0])
		if err != nil || got[0].N != c[1] {
			t.Fatalf("f(%d) = %v, %v; want %d", c[0], got, err, c[1])
		}
	}
	rc := regOf(inst, "f")
	if rc == nil {
		t.Fatal("f runs no register code")
	}
	if op := uint8(rc.code[1]); op != rBrNZ {
		t.Fatalf("the second instruction is %d, want a 32-bit branch", op)
	}
}

// TestRegPollsContext checks that a loop of register code ends when its
// context does.
func TestRegPollsContext(t *testing.T) {
	inst := instantiateReg(t, `(module (func (export "f") (loop $l (br $l))))`)
	ext, _ := inst.Export("f")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Invoke(ctx, ext.Func, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("f() = %v, want the context's error", err)
	}
	if regOf(inst, "f") == nil {
		t.Fatal("f runs no register code")
	}
}

// TestRegWorkloads runs wasmbench's micro workloads at a small size,
// natively and on the Go table, against the golden answers.
func TestRegWorkloads(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "internal", "cmd", "wasmbench", "testdata", "wat")
	for _, w := range []struct {
		file string
		arg  uint64
	}{{"fib.wat", 18}, {"loop.wat", 1000}, {"i64.wat", 1000}, {"globals.wat", 1000}, {"brtable.wat", 1000}, {"bytes.wat", 1}, {"mandelbrot.wat", 20}, {"nbody.wat", 100},
		{"gc.wat", 8}, {"arrays.wat", 1000}, {"simd.wat", 10}, {"indirect.wat", 1000}, {"bulk.wat", 2}} {
		src, err := os.ReadFile(filepath.Join(dir, w.file))
		if err != nil {
			t.Fatal(err)
		}
		t.Run(w.file, func(t *testing.T) {
			regMatchesGolden(t, string(src), []regCall{{"run", []Val{{N: w.arg}}}})
		})
	}
}

const regRefs = `(module
  (type $f (func (result i32)))
  (global $g (mut funcref) (ref.null func))
  (func $one (type $f) (i32.const 1))
  (elem declare func $one)
  (func (export "refs") (param i32) (result i32) (local funcref)
    (local.set 1 (ref.func $one))
    (global.set $g (select (result funcref) (local.get 1) (ref.null func) (local.get 0)))
    (ref.is_null (global.get $g)))
  (func (export "bronnull") (param i32) (result i32) (local funcref)
    (if (local.get 0) (then (local.set 1 (ref.func $one))))
    (block $null (result i32)
      (i32.const 7)
      (br_on_null $null (local.get 1))
      (drop) (drop) (i32.const 9)))
  (func (export "nonnull") (param externref) (result i32)
    (block $nn (result externref)
      (br_on_non_null $nn (local.get 0))
      (return (i32.const 5)))
    (drop) (i32.const 6))
  (func (export "pass") (param externref externref i32) (result externref)
    (select (result externref) (local.get 0) (local.get 1) (local.get 2)))
  (func (export "asnn") (param externref) (result i32) (drop (ref.as_non_null (local.get 0))) (i32.const 1)))`

// TestRegRefs checks register code's references: locals, globals, select,
// ref.func, ref.is_null, br_on_null, br_on_non_null and ref.as_non_null.
func TestRegRefs(t *testing.T) {
	inst := instantiateReg(t, regRefs)
	host := HostRef("x")
	for _, c := range []struct {
		name string
		args []Val
		want Val
	}{
		{"refs", []Val{{N: 1}}, Val{N: 0}},
		{"refs", []Val{{N: 0}}, Val{N: 1}},
		{"bronnull", []Val{{N: 0}}, Val{N: 7}},
		{"bronnull", []Val{{N: 1}}, Val{N: 9}},
		{"nonnull", []Val{{R: Null}}, Val{N: 5}},
		{"nonnull", []Val{{R: host}}, Val{N: 6}},
		{"pass", []Val{{R: host}, {R: Null}, {N: 1}}, Val{R: host}},
		{"pass", []Val{{R: host}, {R: Null}, {N: 0}}, Val{R: Null}},
		{"asnn", []Val{{R: host}}, Val{N: 1}},
	} {
		ext, _ := inst.Export(c.name)
		got, err := Invoke(context.Background(), ext.Func, c.args)
		if err != nil || got[0] != c.want {
			t.Errorf("%s%v = %v, %v; want %v", c.name, c.args, got, err, c.want)
		}
		if regOf(inst, c.name) == nil {
			t.Errorf("%s runs no register code", c.name)
		}
	}
	ext, _ := inst.Export("asnn")
	_, err := Invoke(context.Background(), ext.Func, []Val{{R: Null}})
	var tr *Trap
	if !errors.As(err, &tr) || tr.Code != TrapNullDereference {
		t.Fatalf("asnn(null) = %v, want a null dereference", err)
	}
}

// regCall is a call of an export with its arguments.
type regCall struct {
	name string
	args []Val
}

// TestRegMemories checks the generic accesses: a second memory, an i64
// memory, offsets past 16 bits, and the bulk operations.
func TestRegMemories(t *testing.T) {
	regMatchesGolden(t, `(module
	  (memory $a 1) (memory $b i64 2) (memory $c 1)
	  (data $d (memory $c) (i32.const 0) "hello")
	  (func (export "multi") (param i32) (result i64)
	    (i32.store $c offset=70000 (i32.const 0) (local.get 0))
	    (i64.store $b offset=65536 (i64.const 8) (i64.extend_i32_u (local.get 0)))
	    (i64.add (i64.load32_u $c offset=70000 (i32.const 0)) (i64.load $b (i64.const 65544))))
	  (func (export "big") (param i32) (result i32) (i32.load8_u offset=65537 (local.get 0)))
	  (func (export "over") (param i64) (result i32) (i32.load $b offset=8 (local.get 0)))
	  (func (export "sizes") (result i64)
	    (drop (memory.grow $c (i32.const 1)))
	    (i64.add (i64.add (memory.size $b) (memory.grow $b (i64.const 1))) (i64.extend_i32_u (memory.size $c))))
	  (func (export "bulk") (param i32) (result i32)
	    (memory.init $c $d (i32.const 100) (i32.const 1) (i32.const 4))
	    (memory.copy $a $c (i32.const 10) (i32.const 100) (i32.const 4))
	    (memory.fill $a (i32.const 12) (i32.const 0x41) (local.get 0))
	    (data.drop $d)
	    (i32.load $a (i32.const 10))))`, []regCall{
		{"multi", []Val{{N: 7}}},
		{"big", []Val{{N: 0}}},
		{"big", []Val{{N: 0xffff}}},
		{"over", []Val{{N: math.MaxUint64 - 4}}},
		{"over", []Val{{N: 2*65536 - 4}}},
		{"sizes", nil},
		{"bulk", []Val{{N: 1}}},
		{"bulk", []Val{{N: 1}}},
		{"bulk", []Val{{N: 0x10000}}},
	})
}

// TestRegSIMD checks SIMD operations of each form against the tree's.
func TestRegSIMD(t *testing.T) {
	regMatchesGolden(t, `(module
	  (memory 1)
	  (func (export "ops") (param i32 i64) (result i64)
	    (local v128 v128)
	    (local.set 2 (i32x4.splat (local.get 0)))
	    (local.set 3 (v128.const i8x16 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16))
	    (local.set 2 (i8x16.add (local.get 2) (local.get 3)))
	    (local.set 2 (i16x8.shl (local.get 2) (local.get 0)))
	    (local.set 2 (v128.bitselect (local.get 2) (local.get 3) (v128.const i64x2 -1 0)))
	    (local.set 2 (i8x16.shuffle 0 17 2 19 4 21 6 23 8 25 10 27 12 29 14 31 (local.get 2) (local.get 3)))
	    (local.set 2 (i64x2.replace_lane 1 (local.get 2) (local.get 1)))
	    (local.set 2 (f32x4.add (local.get 2) (f32x4.splat (f32.const 1.5))))
	    (i64.add (i64x2.extract_lane 1 (local.get 2))
	      (i64.extend_i32_s (i32.add (i8x16.extract_lane_s 3 (local.get 2))
	        (i32.add (i8x16.bitmask (local.get 2)) (v128.any_true (local.get 3)))))))
	  (func (export "mem") (param i32) (result i64)
	    (v128.store offset=16 (local.get 0) (v128.const i32x4 1 -2 3 -4))
	    (v128.store8_lane 5 (i32.const 100) (v128.load offset=16 (local.get 0)))
	    (i64.add (i64x2.extract_lane 0 (v128.load32x2_s offset=20 (local.get 0)))
	      (i64x2.extract_lane 1 (v128.load16_lane 7 (i32.const 100) (v128.load32_splat offset=16 (local.get 0))))))
	  (func (export "relaxed") (param f32) (result f32)
	    (f32x4.extract_lane 2 (f32x4.relaxed_madd (f32x4.splat (local.get 0)) (f32x4.splat (local.get 0)) (f32x4.splat (f32.const 2))))))`, []regCall{
		{"ops", []Val{{N: 3}, {N: 1 << 40}}},
		{"ops", []Val{{N: 0xffffffff}, {N: 5}}},
		{"mem", []Val{{N: 0}}},
		{"mem", []Val{{N: 65530}}},
		{"relaxed", []Val{{N: uint64(math.Float32bits(1.25))}}},
	})
}

// TestRegTables checks the table instructions and the calls through tables
// and references, tail calls included, with their traps.
func TestRegTables(t *testing.T) {
	regMatchesGolden(t, `(module
	  (type $ii (func (param i32) (result i32)))
	  (type $v (func))
	  (table $t 4 (ref null $ii)) (table $u i64 2 10 (ref null $ii))
	  (elem $e (ref $ii) (ref.func $dbl) (ref.func $inc))
	  (func $dbl (type $ii) (i32.mul (local.get 0) (i32.const 2)))
	  (func $inc (type $ii) (i32.add (local.get 0) (i32.const 1)))
	  (func (export "ind") (param i32 i32) (result i32)
	    (table.init $t $e (i32.const 0) (i32.const 0) (i32.const 2))
	    (call_indirect $t (type $ii) (local.get 1) (local.get 0)))
	  (func (export "ops") (param i32) (result i64)
	    (table.init $t $e (i32.const 1) (i32.const 0) (i32.const 2))
	    (elem.drop $e)
	    (table.copy $u $t (i64.const 0) (i32.const 1) (i32.const 2))
	    (drop (table.grow $u (table.get $t (i32.const 2)) (i64.const 3)))
	    (table.fill $t (i32.const 3) (ref.null $ii) (local.get 0))
	    (table.set $u (i64.const 4) (ref.func $dbl))
	    (i64.add (table.size $u) (i64.extend_i32_u (call_ref $ii (i32.const 20) (table.get $u (i64.const 4))))))
	  (func (export "tailind") (param i32) (result i32)
	    (table.init $t $e (i32.const 0) (i32.const 0) (i32.const 2))
	    (return_call_indirect $t (type $ii) (i32.const 5) (local.get 0)))
	  (func (export "tailref") (param i32) (result i32)
	    (return_call_ref $ii (local.get 0) (ref.func $inc)))
	  (func (export "wrongtype") (call_indirect $t (type $v) (i32.const 0)))
	  (func (export "nullref") (result i32) (call_ref $ii (i32.const 1) (ref.null $ii))))`, []regCall{
		{"ind", []Val{{N: 0}, {N: 21}}},
		{"ind", []Val{{N: 1}, {N: 21}}},
		{"ind", []Val{{N: 9}, {N: 21}}},
		{"ops", []Val{{N: 1}}},
		{"ops", []Val{{N: 2}}},
		{"tailind", []Val{{N: 1}}},
		{"tailind", []Val{{N: 3}}},
		{"tailref", []Val{{N: 41}}},
		{"wrongtype", nil},
		{"nullref", nil},
	})
}

// TestRegGC checks the GC instructions against the tree's, traps included.
func TestRegGC(t *testing.T) {
	regMatchesGolden(t, `(module
	  (type $p (struct (field (mut i32)) (field (mut i8)) (field (mut v128)) (field (mut anyref))))
	  (type $a (array (mut i16)))
	  (type $r (array (mut eqref)))
	  (data $d "\01\02\03\04\05\06")
	  (elem $e i31ref (ref.i31 (i32.const 7)) (ref.i31 (i32.const -1)))
	  (func (export "structs") (param i32) (result i32)
	    (local $s (ref null $p))
	    (local.set $s (struct.new $p (local.get 0) (i32.const 0x1ff) (v128.const i32x4 1 2 3 4) (ref.i31 (local.get 0))))
	    (struct.set $p 1 (local.get $s) (i32.add (struct.get_s $p 1 (local.get $s)) (i32.const 3)))
	    (i32.add (i32.add (struct.get $p 0 (local.get $s)) (struct.get_u $p 1 (local.get $s)))
	      (i32.add (i32x4.extract_lane 2 (struct.get $p 2 (local.get $s)))
	        (i31.get_s (ref.cast (ref i31) (struct.get $p 3 (local.get $s)))))))
	  (func (export "arrays") (param i32) (result i32)
	    (local $x (ref null $a)) (local $y (ref null $a))
	    (local.set $x (array.new $a (i32.const -2) (local.get 0)))
	    (local.set $y (array.new_data $a $d (i32.const 0) (i32.const 3)))
	    (array.set $a (local.get $x) (i32.const 1) (i32.const 0x12345))
	    (array.fill $a (local.get $x) (i32.const 2) (i32.const 9) (i32.const 2))
	    (array.copy $a $a (local.get $x) (i32.const 0) (local.get $y) (i32.const 1) (i32.const 2))
	    (array.init_data $a $d (local.get $y) (i32.const 0) (i32.const 2) (i32.const 1))
	    (i32.add (i32.add (array.get_s $a (local.get $x) (i32.const 1)) (array.get_u $a (local.get $x) (i32.const 3)))
	      (i32.add (array.len (local.get $x)) (array.get_u $a (local.get $y) (i32.const 0)))))
	  (func (export "refs") (param i32) (result i32)
	    (local $r (ref null $r))
	    (local.set $r (array.new_elem $r $e (i32.const 0) (i32.const 2)))
	    (array.set $r (local.get $r) (i32.const 0) (array.new_fixed $r 1 (ref.null eq)))
	    (block $l (result (ref $r))
	      (br_on_cast $l (ref null eq) (ref $r) (array.get $r (local.get $r) (local.get 0)))
	      (drop)
	      (return (ref.test (ref i31) (array.get $r (local.get $r) (i32.const 1)))))
	    (array.len)))`, []regCall{
		{"structs", []Val{{N: 5}}},
		{"structs", []Val{{N: 0x40000000}}},
		{"arrays", []Val{{N: 4}}},
		{"arrays", []Val{{N: 2}}},
		{"arrays", []Val{{N: 0x7fffffff}}},
		{"refs", []Val{{N: 0}}},
		{"refs", []Val{{N: 1}}},
		{"refs", []Val{{N: 2}}},
	})
}

// TestRegAtomics checks the atomic instructions against the tree's, traps
// included.
func TestRegAtomics(t *testing.T) {
	regMatchesGolden(t, `(module
	  (memory 1 1 shared) (memory $u 1)
	  (func (export "rmw") (param i32 i64) (result i64)
	    (i32.atomic.store (local.get 0) (i32.const 10))
	    (drop (i32.atomic.rmw.add (local.get 0) (i32.const 5)))
	    (drop (i32.atomic.rmw8.cmpxchg_u (local.get 0) (i32.const 15) (i32.const 0x1ff)))
	    (i64.atomic.store offset=8 (local.get 0) (local.get 1))
	    (atomic.fence)
	    (i64.add (i64.atomic.rmw16.xchg_u offset=8 (local.get 0) (i64.const 7))
	      (i64.add (i64.atomic.load offset=8 (local.get 0)) (i64.extend_i32_u (i32.atomic.load (local.get 0))))))
	  (func (export "wait") (param i32) (result i32)
	    (i32.add (memory.atomic.wait32 (local.get 0) (i32.const 1) (i64.const 0))
	      (memory.atomic.notify (local.get 0) (i32.const 2))))
	  (func (export "unshared") (result i32) (memory.atomic.wait64 $u (i32.const 0) (i64.const 0) (i64.const 0))))`, []regCall{
		{"rmw", []Val{{N: 16}, {N: 0x123456789}}},
		{"rmw", []Val{{N: 2}, {N: 1}}},
		{"rmw", []Val{{N: 65536}, {N: 1}}},
		{"wait", []Val{{N: 64}}},
		{"wait", []Val{{N: 3}}},
		{"unshared", nil},
	})
}

// TestRegExceptions checks throw, throw_ref, try_table's clauses to a
// block, a loop and the function, and the legacy try, catch, catch_all,
// rethrow and delegate, nested and across calls, against the tree's.
func TestRegExceptions(t *testing.T) {
	const thrower = `
	  (tag $e (param i32))
	  (tag $f (param i64 v128))
	  (func $thrower (param i32)
	    (if (i32.eqz (local.get 0)) (then (throw $e (i32.const 42))))
	    (if (i32.eq (local.get 0) (i32.const 1)) (then (throw $f (i64.const 7) (v128.const i32x4 1 2 3 4))))
	    (if (i32.eq (local.get 0) (i32.const 2)) (then (unreachable))))`
	regMatchesGolden(t, `(module `+thrower+`
	  (func (export "table") (param i32) (result i64)
	    (block $b (result i32)
	      (block $c (result i64 v128)
	        (try_table (catch $e $b) (catch $f $c)
	          (call $thrower (local.get 0)))
	        (return (i64.const 100)))
	      (i64x2.extract_lane 1)
	      (i64.add)
	      (return))
	    (i64.extend_i32_u))
	  (func (export "loop") (param i32) (result i32) (local i32)
	    (loop $l
	      (try_table (catch_all $l)
	        (local.set 1 (i32.add (local.get 1) (i32.const 1)))
	        (call $thrower (i32.sub (local.get 0) (local.get 1)))))
	    (local.get 1))
	  (func (export "fn") (param i32) (result i32)
	    (try_table (catch $e 0) (call $thrower (local.get 0)))
	    (i32.const -1))
	  (func (export "ref") (param i32) (result i32)
	    (block $h (result exnref)
	      (try_table (catch_all_ref $h) (call $thrower (local.get 0)))
	      (return (i32.const 0)))
	    (block $g (param exnref) (result i32 exnref)
	      (try_table (param exnref) (catch_ref $e $g) (throw_ref))
	      (unreachable))
	    (drop)))`, []regCall{
		{"table", []Val{{N: 0}}},
		{"table", []Val{{N: 1}}},
		{"table", []Val{{N: 2}}},
		{"table", []Val{{N: 3}}},
		{"loop", []Val{{N: 4}}},
		{"fn", []Val{{N: 0}}},
		{"fn", []Val{{N: 3}}},
		{"fn", []Val{{N: 1}}},
		{"ref", []Val{{N: 0}}},
		{"ref", []Val{{N: 1}}},
		{"ref", []Val{{N: 3}}},
	})
	regMatchesGolden(t, `(module `+thrower+`
	  (func (export "legacy") (param i32) (result i32) (local i32)
	    (try (result i32)
	      (do
	        (try (result i32)
	          (do (call $thrower (local.get 0)) (i32.const 5))
	          (catch $e (local.set 1 (i32.const 10)) (rethrow 0))
	          (catch_all (i32.const 6))))
	      (catch $e (i32.add (local.get 1)))))
	  (func (export "delegate") (param i32) (result i32)
	    (try (result i32)
	      (do
	        (try (result i32)
	          (do (try (do (call $thrower (local.get 0))) (delegate 1)) (i32.const 1))
	          (catch_all (i32.const 2))))
	      (catch_all (i32.const 3))))
	  (func (export "outer") (param i32) (result i32)
	    (block $b (result i32)
	      (try (result i32)
	        (do
	          (try (do (call $thrower (local.get 0))) (catch $f (br $b (i32.const 7))))
	          (try (do (call $thrower (i32.const 0))) (catch_all (br $b (i32.const 8))))
	          (i32.const 9))
	        (catch $e (drop) (i32.const 10))))))`, []regCall{
		{"legacy", []Val{{N: 0}}},
		{"legacy", []Val{{N: 1}}},
		{"legacy", []Val{{N: 3}}},
		{"delegate", []Val{{N: 0}}},
		{"delegate", []Val{{N: 3}}},
		{"outer", []Val{{N: 1}}},
		{"outer", []Val{{N: 3}}},
	})
}

// TestRegFarSlots checks a body whose slots pass 16 bits: forty thousand
// v128 locals put every later local and every stack position past what an
// instruction's field holds, so that its instructions run behind the far
// prefix, against the tree's.
func TestRegFarSlots(t *testing.T) {
	src := `(module
	  (memory 1)
	  (global $g (mut i32) (i32.const 0))
	  (table 1 funcref)
	  (func $id (param i32) (result i32) (local.get 0))
	  (func (export "f") (param i32) (result i32)
	    (local` + strings.Repeat(" v128", 40000) + `)
	    (local $a i32) (local $b i32) (local $r funcref) (local $v v128)
	    (local.set $a (i32.add (local.get 0) (i32.const 1000)))
	    (local.set $b (i32.mul (local.get $a) (local.get $a)))
	    (local.set $v (i32x4.splat (local.get $b)))
	    (local.set $v (v128.bitselect (local.get $v) (local.get 1) (local.get $v)))
	    (local.set $r (table.get (i32.const 0)))
	    (block $out
	      (loop $l
	        (br_if $out (i32.ge_u (local.get $a) (i32.const 1010)))
	        (local.set $a (i32.add (local.get $a) (i32.const 1)))
	        (block $x (block $y (br_table $x $y $l (i32.and (local.get $a) (i32.const 1)))))
	        (br $l)))
	    (i32.store (i32.const 8) (local.get $b))
	    (global.set $g (call $id (local.get $a)))
	    (i32.add (ref.is_null (local.get $r))
	      (select (i32.add (i32.load (i32.const 8)) (global.get $g)) (i32x4.extract_lane 0 (local.get $v)) (local.get 0)))))`
	regMatchesGolden(t, src, []regCall{{"f", []Val{{N: 1}}}, {"f", []Val{{N: 0}}}, {"f", []Val{{N: 5}}}})
	inst := instantiateReg(t, src)
	if _, err := call(t, inst, "f", 1); err != nil {
		t.Fatal(err)
	}
	far := 0
	for _, w := range regOf(inst, "f").code {
		if uint8(w) == rFar {
			far++
		}
	}
	if far == 0 {
		t.Fatal("no instruction behind a far prefix")
	}
}
