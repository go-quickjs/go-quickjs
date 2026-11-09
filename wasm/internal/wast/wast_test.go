package wast

import (
	"bytes"
	"reflect"
	"testing"
)

const sample = `;; A script with every kind of command.
(module $M (func (export "f") (param i32) (result i32) (local.get 0)))
(module definition $D (memory 1))
(module instance $I $D)
(module instance $D)
(register "m" $M)
(invoke "f" (i32.const -1))
(assert_return (invoke $M "f" (i32.const 7)) (i32.const 7))
(assert_return (get "g") (f32.const nan:canonical) (f64.const nan:arithmetic) (f32.const -0x1p-1))
(assert_return (invoke "v" (v128.const i16x8 1 -1 2 3 4 5 6 0xffff))
  (v128.const f32x4 nan:canonical 1.5 nan:arithmetic -0))
(assert_return (invoke "r" (ref.null func) (ref.extern 3) (ref.host 4))
  (ref.null) (ref.null extern) (ref.extern 5) (ref.extern) (ref.func) (ref.i31) (ref.struct)
  (ref.array) (ref.eq) (ref) (ref.exn) (ref.host 6)
  (either (i32.const 1) (i64.const -1)))
(assert_trap (invoke "t") "unreachable")
(assert_trap (module (func $f unreachable) (start $f)) "unreachable")
(assert_exhaustion (invoke "e") "call stack exhausted")
(assert_exception (invoke "x"))
(assert_invalid (module (func (result i32))) "type mismatch")
(assert_malformed (module quote "(func" " i32.const)") "unexpected token")
(assert_malformed (module binary "\00asm" "\02\00\00\00") "unknown binary version")
(assert_unlinkable (module (import "m" "missing" (func))) "unknown import")
(assert_unlinkable (module instance $D) "incompatible import")
(thread $T (shared (module $M))
  (register "m" $M)
  (assert_return (invoke $M "f" (i32.const 1)) (i32.const 1)))
(wait $T)
`

func TestParse(t *testing.T) {
	cmds, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	var lines []int
	for _, c := range cmds {
		lines = append(lines, c.Line())
	}
	if want := []int{2, 3, 4, 5, 6, 7, 8, 9, 10, 12, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 28}; !reflect.DeepEqual(lines, want) {
		t.Fatalf("lines %v, want %v", lines, want)
	}

	m := cmds[0].(*ModuleCommand)
	bin, err := m.Module.Binary()
	if m.Definition || m.Module.Name != "M" || m.Module.Kind != TextModule || err != nil || !bytes.HasPrefix(bin, []byte("\x00asm")) {
		t.Errorf("module %+v, %v", m, err)
	}
	if d := cmds[1].(*ModuleCommand); !d.Definition || d.Module.Name != "D" {
		t.Errorf("definition %+v", d)
	}
	if i := cmds[2].(*InstanceCommand); i.Instance != "I" || i.Module != "D" {
		t.Errorf("instance %+v", i)
	}
	if i := cmds[3].(*InstanceCommand); i.Instance != "" || i.Module != "D" {
		t.Errorf("instance %+v", i)
	}
	if r := cmds[4].(*RegisterCommand); r.As != "m" || r.Instance != "M" {
		t.Errorf("register %+v", r)
	}
	if a := cmds[5].(*ActionCommand); a.Action.Kind != Invoke || a.Action.Name != "f" ||
		!reflect.DeepEqual(a.Action.Args, []Value{{Kind: I32, Bits: 0xffffffff}}) {
		t.Errorf("action %+v", a)
	}

	r := cmds[6].(*AssertReturn)
	if r.Action.Instance != "M" || !reflect.DeepEqual(r.Results, []Result{{Kind: ResultValue, Value: Value{Kind: I32, Bits: 7}}}) {
		t.Errorf("assert_return %+v", r)
	}
	r = cmds[7].(*AssertReturn)
	want := []Result{
		{Kind: ResultNaN, NaN: CanonicalNaN, Value: Value{Kind: F32}},
		{Kind: ResultNaN, NaN: ArithmeticNaN, Value: Value{Kind: F64}},
		{Kind: ResultValue, Value: Value{Kind: F32, Bits: 0xbf000000}},
	}
	if r.Action.Kind != Get || !reflect.DeepEqual(r.Results, want) {
		t.Errorf("assert_return %+v", r.Results)
	}

	r = cmds[8].(*AssertReturn)
	arg := r.Action.Args[0]
	if arg.Kind != V128 || arg.Shape != I16x8 ||
		arg.V128 != [16]byte{1, 0, 0xff, 0xff, 2, 0, 3, 0, 4, 0, 5, 0, 6, 0, 0xff, 0xff} {
		t.Errorf("v128 argument %+v", arg)
	}
	v := r.Results[0]
	if v.Kind != ResultV128 || v.Value.Shape != F32x4 || !reflect.DeepEqual(v.Lanes, []Lane{
		{NaN: CanonicalNaN}, {Bits: 0x3fc00000}, {NaN: ArithmeticNaN}, {Bits: 0x80000000},
	}) {
		t.Errorf("v128 result %+v", v)
	}

	r = cmds[9].(*AssertReturn)
	if !reflect.DeepEqual(r.Action.Args, []Value{{Kind: RefNull, Heap: "func"}, {Kind: RefExtern, Ref: 3}, {Kind: RefHost, Ref: 4}}) {
		t.Errorf("reference arguments %+v", r.Action.Args)
	}
	wantRefs := []Result{
		{Kind: ResultRefNull}, {Kind: ResultRefNull, Heap: "extern"},
		{Kind: ResultValue, Value: Value{Kind: RefExtern, Ref: 5}}, {Kind: ResultRef, Heap: "extern"},
		{Kind: ResultRef, Heap: "func"}, {Kind: ResultRef, Heap: "i31"}, {Kind: ResultRef, Heap: "struct"},
		{Kind: ResultRef, Heap: "array"}, {Kind: ResultRef, Heap: "eq"}, {Kind: ResultRef, Heap: "any"},
		{Kind: ResultRef, Heap: "exn"}, {Kind: ResultValue, Value: Value{Kind: RefHost, Ref: 6}},
		{Kind: ResultEither, Either: []Result{
			{Kind: ResultValue, Value: Value{Kind: I32, Bits: 1}},
			{Kind: ResultValue, Value: Value{Kind: I64, Bits: 0xffffffffffffffff}},
		}},
	}
	if !reflect.DeepEqual(r.Results, wantRefs) {
		t.Errorf("reference results\n got %+v\nwant %+v", r.Results, wantRefs)
	}

	if a := cmds[10].(*AssertTrap); a.Action.Name != "t" || a.Message != "unreachable" {
		t.Errorf("assert_trap %+v", a)
	}
	if a := cmds[11].(*AssertUninstantiable); a.Module == nil || a.Message != "unreachable" {
		t.Errorf("assert_trap of a module %+v", a)
	}
	if a := cmds[12].(*AssertExhaustion); a.Message != "call stack exhausted" {
		t.Errorf("assert_exhaustion %+v", a)
	}
	if a := cmds[13].(*AssertException); a.Action.Name != "x" {
		t.Errorf("assert_exception %+v", a)
	}
	if a := cmds[14].(*AssertInvalid); a.Message != "type mismatch" || a.Module.Kind != TextModule {
		t.Errorf("assert_invalid %+v", a)
	}
	q := cmds[15].(*AssertMalformed)
	if q.Module.Kind != QuoteModule || string(q.Module.Text) != "(func i32.const)" {
		t.Errorf("quoted module %+v", q.Module)
	}
	if _, err := q.Module.Binary(); err == nil {
		t.Errorf("malformed quoted module compiled")
	}
	b := cmds[16].(*AssertMalformed)
	if bin, _ := b.Module.Binary(); b.Module.Kind != BinaryModule || string(bin) != "\x00asm\x02\x00\x00\x00" {
		t.Errorf("binary module %+v", b.Module)
	}
	if a := cmds[17].(*AssertUnlinkable); a.Module == nil || a.Message != "unknown import" {
		t.Errorf("assert_unlinkable %+v", a)
	}
	if a := cmds[18].(*AssertUnlinkable); a.Module != nil || a.Definition != "D" {
		t.Errorf("assert_unlinkable of a definition %+v", a)
	}
	th := cmds[19].(*ThreadCommand)
	if th.Name != "T" || !reflect.DeepEqual(th.Shared, []string{"M"}) || len(th.Commands) != 2 || th.Commands[1].Line() != 27 {
		t.Errorf("thread %+v", th)
	}
	if w := cmds[20].(*WaitCommand); w.Thread != "T" {
		t.Errorf("wait %+v", w)
	}
}

func TestParseInlineModule(t *testing.T) {
	cmds, err := Parse([]byte("(func (export \"f\"))\n(memory 1)"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 1 {
		t.Fatalf("%d commands", len(cmds))
	}
	if m := cmds[0].(*ModuleCommand); m.Module.Kind != TextModule || m.Definition {
		t.Errorf("module %+v", m)
	}
}

func TestParseErrors(t *testing.T) {
	for _, src := range []string{
		`(module (func (i32.const x)))`,
		`(assert_return (invoke "f" (i32.const 1.5)))`,
		`(assert_return (invoke "f") (i32.const nan:canonical))`,
		`(assert_return (invoke "f") (v128.const i32x4 1 2 3))`,
		`(assert_return (invoke "f") (v128.const i32x4 nan:canonical 1 2 3))`,
		`(assert_trap (module definition (func)) "x")`,
		`(frobnicate)`,
		`(invoke "f"`,
	} {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("Parse(%q) succeeded", src)
		}
	}
}
