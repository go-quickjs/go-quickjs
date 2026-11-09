package wat

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type section struct {
	id   byte
	name string // a custom section's name
	data []byte // the contents, a custom section's after its name
}

func readU32(t *testing.T, b []byte, i *int) uint32 {
	t.Helper()
	var v uint32
	for shift := 0; ; shift += 7 {
		if *i >= len(b) {
			t.Fatal("truncated LEB128")
		}
		c := b[*i]
		*i++
		v |= uint32(c&0x7f) << shift
		if c&0x80 == 0 {
			return v
		}
	}
}

func readName(t *testing.T, b []byte, i *int) string {
	n := int(readU32(t, b, i))
	s := string(b[*i : *i+n])
	*i += n
	return s
}

// sections splits a binary module into its sections.
func sections(t *testing.T, bin []byte) []section {
	t.Helper()
	if !bytes.HasPrefix(bin, []byte("\x00asm\x01\x00\x00\x00")) {
		t.Fatalf("bad header % x", bin[:8])
	}
	var ss []section
	for i := 8; i < len(bin); {
		s := section{id: bin[i]}
		i++
		n := int(readU32(t, bin, &i))
		s.data = bin[i : i+n]
		i += n
		if s.id == 0 {
			j := 0
			s.name = readName(t, s.data, &j)
			s.data = s.data[j:]
		}
		ss = append(ss, s)
	}
	return ss
}

func compile(t *testing.T, src string) []byte {
	t.Helper()
	bin, err := Compile([]byte(src))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return bin
}

// withoutNames returns bin's hex without its name section.
func withoutNames(t *testing.T, bin []byte) string {
	var out []byte
	out = append(out, bin[:8]...)
	for _, s := range sections(t, bin) {
		if s.id == 0 && s.name == "name" {
			continue
		}
		content := s.data
		if s.id == 0 {
			content = append(append([]byte{byte(len(s.name))}, s.name...), s.data...)
		}
		out = append(out, s.id, byte(len(content)))
		out = append(out, content...)
	}
	return hex.EncodeToString(out)
}

func TestCompileEncodings(t *testing.T) {
	tests := []struct{ src, want string }{
		{`(module)`, "0061736d01000000"},
		{``, "0061736d01000000"},
		// A function with an inline export; the folded and flat forms.
		{`(module (func (export "f") (param i32) (result i32) local.get 0))`,
			"0061736d01000000" + "01060160017f017f" + "03020100" + "070501016600" + "00" + "0a0601040020000b"},
		{`(func (export "f") (param i32) (result i32) (local.get 0))`,
			"0061736d01000000" + "01060160017f017f" + "03020100" + "070501016600" + "00" + "0a0601040020000b"},
		// Inline imports come first in their index space.
		{`(module (func (import "m" "f") (param i32)) (global (export "g") (import "m" "g") (mut i64)))`,
			"0061736d01000000" + "0105016001" + "7f00" + "02" + "0e" + "02016d016600" + "00" + "016d0167037e01" + "070501016703" + "00"},
		// A memory's inline data, and a table's inline elements.
		{`(module (memory (data "ab")) (func) (table funcref (elem 0 0)))`,
			"0061736d01000000" + "01040160" + "0000" + "03020100" + "0405017001" + "0202" + "0504010101" + "01" +
				"090c" + "0104" + "41000b" + "02" + "d2000b" + "d2000b" + "0a040102000b" + "0b08010041000b026162"},
		// A 64-bit table's inline elements and a table initializer.
		{`(module (table i64 funcref (elem)) (table 2 externref (ref.null extern)) (table 3 (ref null func) (ref.null func)) (table 1 (ref func) (ref.func 0)) (func))`,
			"0061736d01000000" + "01040160" + "0000" + "03020100" +
				"04" + "14" + "04" + "70050000" + "6f0002" + "700003" + "40006470" + "0001" + "d2000b" +
				"0906" + "01" + "04" + "42000b" + "00" + "0a040102000b"},
		// Memory arguments: alignment, offset and memory index.
		{`(module (memory 1) (memory $m i64 1 2 shared) (func (drop (i32.load $m offset=8 align=2 (i64.const 0)))))`,
			"0061736d01000000" + "01040160" + "0000" + "03020100" + "0506" + "02" + "0001" + "070102" +
				"0a0b010900" + "4200" + "28410108" + "1a0b"},
		// Blocks with value and inline types, and labels.
		{`(module (func (block $a (result i32) (br $a (i32.const 1))) drop (loop $l (br_if $l (i32.const 0)))))`,
			"0061736d01000000" + "01040160" + "0000" + "03020100" +
				"0a" + "13" + "01" + "11" + "00" + "027f" + "4101" + "0c00" + "0b" + "1a" + "0340" + "4100" + "0d00" + "0b" + "0b"},
	}
	for _, tt := range tests {
		got := withoutNames(t, compile(t, tt.src))
		if got != tt.want {
			t.Errorf("%s\n got %s\nwant %s", tt.src, got, tt.want)
		}
	}
}

// funcTypes decodes a type section of function types without references.
func funcTypes(t *testing.T, bin []byte) []string {
	for _, s := range sections(t, bin) {
		if s.id != 1 {
			continue
		}
		i := 0
		n := int(readU32(t, s.data, &i))
		var out []string
		names := map[byte]string{0x7f: "i32", 0x7e: "i64", 0x7d: "f32", 0x7c: "f64"}
		for k := 0; k < n; k++ {
			if s.data[i] != 0x60 {
				t.Fatalf("type %d is not a function type", k)
			}
			i++
			var parts []string
			for side := 0; side < 2; side++ {
				m := int(readU32(t, s.data, &i))
				var ts []string
				for j := 0; j < m; j++ {
					ts = append(ts, names[s.data[i]])
					i++
				}
				parts = append(parts, "["+strings.Join(ts, " ")+"]")
			}
			out = append(out, parts[0]+"->"+parts[1])
		}
		return out
	}
	return nil
}

// TestImplicitTypeOrder checks that inline function types are added to the
// type section in the reference interpreter's order: after every explicit
// type, field by field; within a function its signature first, then its
// instructions last to first, a block's body before its own type, a folded
// if's else branch before its then branch, and a folded call_indirect's
// operands before its own type.
func TestImplicitTypeOrder(t *testing.T) {
	bin := compile(t, `(module
  (func
    (block (result i32 i32) (i32.const 1) (i32.const 2)) drop drop
    (block (result f32 f32) (f32.const 1) (f32.const 2)) drop drop)
  (type (func))
  (func (result i32)
    (if (result i32 i32) (i32.const 1)
      (then (block (result i64 i64) (i64.const 1) (i64.const 2)) drop drop (i32.const 1) (i32.const 2))
      (else (block (result f64 f64) (f64.const 1) (f64.const 2)) drop drop (i32.const 3) (i32.const 4)))
    drop)
  (func (param i32)
    (call_indirect (param i32 i32) (result i64 f64)
      (block (result i64 i32) (i64.const 0) (i32.const 0))
      (local.get 0))
    drop drop
    call_indirect (param f32 f32 f32)))`)
	got := funcTypes(t, bin)
	want := []string{
		"[]->[]",            // the explicit type, which the first function's signature uses
		"[]->[f32 f32]",     // the first function's second block
		"[]->[i32 i32]",     // its first block, and later the if
		"[]->[i32]",         // the second function's signature
		"[]->[f64 f64]",     // the if's else branch
		"[]->[i64 i64]",     // its then branch
		"[i32]->[]",         // the third function's signature
		"[f32 f32 f32]->[]", // its flat call_indirect, the last instruction
		"[]->[i64 i32]",     // the folded call_indirect's operand
		"[i32 i32]->[i64 f64]",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("types\n got %q\nwant %q", got, want)
	}
}

// TestInlineTypeReuse checks that an inline signature takes the first
// matching type that is final, without supertypes and alone in its
// recursion group, and no other.
func TestInlineTypeReuse(t *testing.T) {
	bin := compile(t, `(module
  (type (sub (func)))
  (rec (type (func)) (type (struct)))
  (type $f (sub final (func)))
  (func))`)
	for _, s := range sections(t, bin) {
		if s.id == 3 && !bytes.Equal(s.data, []byte{1, 3}) {
			t.Errorf("function section % x, want type 3", s.data)
		}
	}
}

type names struct {
	module string
	maps   map[byte]map[uint32]string
	nested map[byte]map[uint32]map[uint32]string
}

func decodeNames(t *testing.T, bin []byte) names {
	n := names{maps: map[byte]map[uint32]string{}, nested: map[byte]map[uint32]map[uint32]string{}}
	for _, s := range sections(t, bin) {
		if s.id != 0 || s.name != "name" {
			continue
		}
		last := -1
		for i := 0; i < len(s.data); {
			id := s.data[i]
			if int(id) <= last {
				t.Fatalf("name subsection %d after %d", id, last)
			}
			last = int(id)
			i++
			size := int(readU32(t, s.data, &i))
			sub := s.data[i : i+size]
			i += size
			j := 0
			nameMap := func() map[uint32]string {
				m := map[uint32]string{}
				count := int(readU32(t, sub, &j))
				for k := 0; k < count; k++ {
					x := readU32(t, sub, &j)
					m[x] = readName(t, sub, &j)
				}
				return m
			}
			switch id {
			case 0:
				n.module = readName(t, sub, &j)
			case 2, 10:
				m := map[uint32]map[uint32]string{}
				count := int(readU32(t, sub, &j))
				for k := 0; k < count; k++ {
					x := readU32(t, sub, &j)
					m[x] = nameMap()
				}
				n.nested[id] = m
			default:
				n.maps[id] = nameMap()
			}
			if j != len(sub) {
				t.Fatalf("name subsection %d has %d bytes left", id, len(sub)-j)
			}
		}
	}
	return n
}

func TestNameSection(t *testing.T) {
	bin := compile(t, `(module $m
  (type $t (struct (field $a i32) (field $b i64)))
  (import "env" "f" (func $imp))
  (func $g (param $x i32) (param i64) (local $y i64) (local f32))
  (func (local $z i32))
  (table $tab 1 funcref) (memory $mem 1) (global $glob i32 (i32.const 0))
  (tag $e) (elem $el func) (data $d ""))`)
	got := decodeNames(t, bin)
	want := names{
		module: "m",
		maps: map[byte]map[uint32]string{
			1: {0: "imp", 1: "g"}, 4: {0: "t"}, 5: {0: "tab"}, 6: {0: "mem"},
			7: {0: "glob"}, 8: {0: "el"}, 9: {0: "d"}, 11: {0: "e"},
		},
		nested: map[byte]map[uint32]map[uint32]string{
			2:  {1: {0: "x", 2: "y"}, 2: {0: "z"}},
			10: {0: {0: "a", 1: "b"}},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("names\n got %+v\nwant %+v", got, want)
	}
	// A module without identifiers has no name section.
	for _, s := range sections(t, compile(t, `(module (func (local i32)))`)) {
		if s.id == 0 {
			t.Errorf("unexpected custom section %q", s.name)
		}
	}
}

func TestNameAnnotations(t *testing.T) {
	bin := compile(t, `(module $moduel (@name "Modül")
  (type $t (func))
  (func (@name "λ") (type $t))
  (func $lambda (@name "μ") (type $t))
  (tag $theta (@name "θ") (type $t)))`)
	got := decodeNames(t, bin)
	if got.module != "Modül" || !reflect.DeepEqual(got.maps[1], map[uint32]string{0: "λ", 1: "μ"}) ||
		!reflect.DeepEqual(got.maps[11], map[uint32]string{0: "θ"}) {
		t.Errorf("names %+v", got)
	}
	for _, src := range []string{
		`(module (@name "M1") (@name "M2"))`,
		`(module (func) (@name "M"))`,
		`(module (start $f (@name "M")) (func $f))`,
		`(module (func (nop (@name "x"))))`,
	} {
		if _, err := Compile([]byte(src)); err == nil {
			t.Errorf("%s: compiled", src)
		}
	}
}

func TestCustomAnnotations(t *testing.T) {
	bin := compile(t, `(module
  (type $t (func))
  (@custom "my-section1" "contents-bytes1")
  (@custom "my-section2" "more-contents-bytes0")
  (@custom "my-section1" "contents-bytes2")
  (@custom "my-section2" (before global) "more-contents-bytes1")
  (@custom "my-section2" (after func) "more-contents-bytes2")
  (@custom "my-section2" (after func) "more-contents-bytes3")
  (@custom "my-section2" (before global) "more-contents-bytes4")
  (func)
  (@custom "my-section2" "more-contents-bytes5")
  (global $g i32 (i32.const 0))
  (@custom "my-section3")
  (@custom "my-section4" "" "1" "" "2" "3" "")
  (@custom "first" (before first) "x")
  (@custom ""))`)
	var got []string
	for _, s := range sections(t, bin) {
		if s.id == 0 {
			got = append(got, fmt.Sprintf("%s:%s", s.name, s.data))
		} else {
			got = append(got, fmt.Sprint(s.id))
		}
	}
	want := []string{
		"first:x", "1", "3",
		"my-section2:more-contents-bytes2", "my-section2:more-contents-bytes3",
		"my-section2:more-contents-bytes1", "my-section2:more-contents-bytes4",
		"6", "10",
		"my-section1:contents-bytes1", "my-section2:more-contents-bytes0",
		"my-section1:contents-bytes2", "my-section2:more-contents-bytes5",
		"my-section3:", "my-section4:123", ":",
		"name:" + string(sections(t, bin)[len(sections(t, bin))-1].data),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sections\n got %q\nwant %q", got, want)
	}
	for _, src := range []string{
		`(@custom)`, `(@custom 4)`, `(@custom "\df")`, `(@custom "bla" here)`,
		`(@custom "bla" (after))`, `(@custom "bla" (type))`, `(@custom "bla" (before types))`,
		`(type (@custom "bla") $t (func))`, `(func (block (@custom "bla")))`,
	} {
		if _, err := Compile([]byte(src)); err == nil {
			t.Errorf("%s: compiled", src)
		}
	}
	// Other annotations are ignored, wherever they are.
	compile(t, `((@a) module (@a) $m (@a) ((@a) func (@a) (@b (c) "d" 1) (nop) (@x)))`)
}

func TestCompileErrors(t *testing.T) {
	tests := []struct {
		src       string
		line, col int
	}{
		{"(module\n  (func (i32.const 1.5)))", 2, 20},
		{"(module (func (local.get $x)))", 1, 26},
		{"(module (func $f) (func $f))", 1, 25},
		{"(module (func block $a end $b))", 1, 28},
		{"(module (memory 1) (func (i32.load align=3 (i32.const 0))))", 1, 36},
		{"(module\n(func (i32.const 0x)))", 2, 18},
		{"(module (type $t (func)) (func (type $t) (param i32)))", 1, 32},
		{"(module (func) (import \"m\" \"n\" (func)))", 1, 16},
		{`(module (data "\u{d800}"))`, 1, 16},
		{"(module (; unclosed", 1, 9},
		{"(module (func (nop))", 1, 21},
		{"(module (func (f32.const 1e39)))", 1, 26},
		{"(module (func (f32.const nan:0x800000)))", 1, 26},
		{"(module (func (i32.const 4294967296)))", 1, 26},
		{"(module (func (i32.const -2147483649)))", 1, 26},
		{"(module (func (v128.const i32x4 1 2 3)))", 1, 38},
	}
	for _, tt := range tests {
		_, err := Compile([]byte(tt.src))
		var e *Error
		if !errors.As(err, &e) {
			t.Errorf("%q: error %v, want an *Error", tt.src, err)
			continue
		}
		if e.Line != tt.line || e.Col != tt.col {
			t.Errorf("%q: error at %d:%d (%v), want %d:%d", tt.src, e.Line, e.Col, e, tt.line, tt.col)
		}
	}
}
