package text

import (
	"encoding/binary"
	"strings"
)

type nodeKind uint8

const (
	nPlain        nodeKind = iota // an instruction encoded when parsed
	nBlock                        // block, loop, if, try_table or try
	nCallIndirect                 // call_indirect, whose type may be implicit
)

// node is a parsed instruction. A folded instruction keeps its operands,
// which are emitted before it; for a folded if they are its condition.
type node struct {
	kind     nodeKind
	tok      Token
	enc      []byte
	folded   bool
	operands []*node

	// Facts that table and element encodings look at.
	isRefNull bool
	heap      int64
	isRefFunc bool
	funcIdx   uint32

	// Blocks.
	op       byte
	bt       blockType
	body     []*node
	els      []*node
	catches  []byte
	legacy   []legacyCatch
	delegate bool
	target   uint32

	// call_indirect and return_call_indirect.
	ret   bool
	table uint32
}

// blockType is a block's type, or a call_indirect's: an explicit type index
// and an inline signature. Which it encodes as is decided when implicit
// types are allocated.
type blockType struct {
	tok      Token
	explicit bool
	idx      uint32
	sig      funcSig
	enc      []byte
}

type legacyCatch struct {
	all  bool
	tag  uint32
	body []*node
}

// funcCtx is the context instructions are parsed in: the locals and the
// labels, innermost last.
type funcCtx struct {
	locals space
	labels []string
}

func (f *funcCtx) push(label string) { f.labels = append(f.labels, label) }
func (f *funcCtx) pop()              { f.labels = f.labels[:len(f.labels)-1] }

func (c *compiler) label(f *funcCtx) uint32 {
	t := c.peek()
	if t.Kind == ID {
		c.p++
		for i := len(f.labels) - 1; i >= 0; i-- {
			if f.labels[i] == t.Text {
				return uint32(len(f.labels) - 1 - i)
			}
		}
		c.fail(t, "unknown label $%s", t.Text)
	}
	return c.nat32()
}

func (c *compiler) local(f *funcCtx) uint32 { return c.index(&f.locals) }

// opcode encodings.

func (o opcode) encode(b []byte) []byte {
	if o.prefix == 0 {
		return append(b, byte(o.code))
	}
	return appendU32(append(b, o.prefix), o.code)
}

func op0(code byte) opcode                { return opcode{code: uint32(code)} }
func opP(prefix byte, code uint32) opcode { return opcode{prefix: prefix, code: code} }

// memOp is a memory instruction: its opcode and natural alignment (log2).
type memOp struct {
	op    opcode
	align uint32
	lane  bool
}

var memOps = map[string]memOp{}

func init() {
	plain := []struct {
		name  string
		code  byte
		align uint32
	}{
		{"i32.load", 0x28, 2}, {"i64.load", 0x29, 3}, {"f32.load", 0x2a, 2}, {"f64.load", 0x2b, 3},
		{"i32.load8_s", 0x2c, 0}, {"i32.load8_u", 0x2d, 0}, {"i32.load16_s", 0x2e, 1}, {"i32.load16_u", 0x2f, 1},
		{"i64.load8_s", 0x30, 0}, {"i64.load8_u", 0x31, 0}, {"i64.load16_s", 0x32, 1}, {"i64.load16_u", 0x33, 1},
		{"i64.load32_s", 0x34, 2}, {"i64.load32_u", 0x35, 2},
		{"i32.store", 0x36, 2}, {"i64.store", 0x37, 3}, {"f32.store", 0x38, 2}, {"f64.store", 0x39, 3},
		{"i32.store8", 0x3a, 0}, {"i32.store16", 0x3b, 1}, {"i64.store8", 0x3c, 0}, {"i64.store16", 0x3d, 1},
		{"i64.store32", 0x3e, 2},
	}
	for _, m := range plain {
		memOps[m.name] = memOp{op: op0(m.code), align: m.align}
	}
	simd := []struct {
		name  string
		code  uint32
		align uint32
		lane  bool
	}{
		{"v128.load", 0x00, 4, false}, {"v128.load8x8_s", 0x01, 3, false}, {"v128.load8x8_u", 0x02, 3, false},
		{"v128.load16x4_s", 0x03, 3, false}, {"v128.load16x4_u", 0x04, 3, false},
		{"v128.load32x2_s", 0x05, 3, false}, {"v128.load32x2_u", 0x06, 3, false},
		{"v128.load8_splat", 0x07, 0, false}, {"v128.load16_splat", 0x08, 1, false},
		{"v128.load32_splat", 0x09, 2, false}, {"v128.load64_splat", 0x0a, 3, false},
		{"v128.store", 0x0b, 4, false}, {"v128.load32_zero", 0x5c, 2, false}, {"v128.load64_zero", 0x5d, 3, false},
		{"v128.load8_lane", 0x54, 0, true}, {"v128.load16_lane", 0x55, 1, true},
		{"v128.load32_lane", 0x56, 2, true}, {"v128.load64_lane", 0x57, 3, true},
		{"v128.store8_lane", 0x58, 0, true}, {"v128.store16_lane", 0x59, 1, true},
		{"v128.store32_lane", 0x5a, 2, true}, {"v128.store64_lane", 0x5b, 3, true},
	}
	for _, m := range simd {
		memOps[m.name] = memOp{op: opP(0xfd, m.code), align: m.align, lane: m.lane}
	}
	// The threads proposal's atomic memory instructions.
	atomics := []struct {
		name  string
		code  uint32
		align uint32
	}{
		{"memory.atomic.notify", 0x00, 2}, {"memory.atomic.wait32", 0x01, 2}, {"memory.atomic.wait64", 0x02, 3},
		{"i32.atomic.load", 0x10, 2}, {"i64.atomic.load", 0x11, 3},
		{"i32.atomic.load8_u", 0x12, 0}, {"i32.atomic.load16_u", 0x13, 1},
		{"i64.atomic.load8_u", 0x14, 0}, {"i64.atomic.load16_u", 0x15, 1}, {"i64.atomic.load32_u", 0x16, 2},
		{"i32.atomic.store", 0x17, 2}, {"i64.atomic.store", 0x18, 3},
		{"i32.atomic.store8", 0x19, 0}, {"i32.atomic.store16", 0x1a, 1},
		{"i64.atomic.store8", 0x1b, 0}, {"i64.atomic.store16", 0x1c, 1}, {"i64.atomic.store32", 0x1d, 2},
	}
	for _, m := range atomics {
		memOps[m.name] = memOp{op: opP(0xfe, m.code), align: m.align}
	}
	for i, rmw := range []string{"add", "sub", "and", "or", "xor", "xchg", "cmpxchg"} {
		base := uint32(0x1e + 7*i)
		forms := []struct {
			name  string
			align uint32
		}{
			{"i32.atomic.rmw." + rmw, 2}, {"i64.atomic.rmw." + rmw, 3},
			{"i32.atomic.rmw8." + rmw + "_u", 0}, {"i32.atomic.rmw16." + rmw + "_u", 1},
			{"i64.atomic.rmw8." + rmw + "_u", 0}, {"i64.atomic.rmw16." + rmw + "_u", 1},
			{"i64.atomic.rmw32." + rmw + "_u", 2},
		}
		for j, f := range forms {
			memOps[f.name] = memOp{op: opP(0xfe, base+uint32(j)), align: f.align}
		}
	}
	for name := range simpleOps {
		instrNames[name] = true
	}
	for name := range memOps {
		instrNames[name] = true
	}
	for _, name := range strings.Fields(otherInstrs) {
		instrNames[name] = true
	}
}

const otherInstrs = `unreachable nop drop select block loop if try_table try br br_if br_table
br_on_null br_on_non_null br_on_cast br_on_cast_fail return call call_ref call_indirect
return_call return_call_ref return_call_indirect throw throw_ref rethrow
local.get local.set local.tee global.get global.set
table.get table.set table.size table.grow table.fill table.copy table.init elem.drop
memory.size memory.grow memory.fill memory.copy memory.init data.drop
i32.const i64.const f32.const f64.const v128.const i8x16.shuffle
ref.null ref.func ref.is_null ref.as_non_null ref.test ref.cast ref.eq ref.i31
struct.new struct.new_default struct.get struct.get_s struct.get_u struct.set
array.new array.new_default array.new_fixed array.new_data array.new_elem
array.get array.get_s array.get_u array.set array.len array.fill array.copy
array.init_data array.init_elem atomic.fence`

// instrNames are the keywords that start an instruction.
var instrNames = map[string]bool{}

// instrs parses instructions up to a token that cannot start one.
func (c *compiler) instrs(f *funcCtx) []*node {
	var list []*node
	for {
		t := c.peek()
		switch t.Kind {
		case LParen:
			k := c.at(c.p + 1)
			if k.Kind != Keyword || !instrNames[k.Text] {
				return list
			}
			list = append(list, c.folded(f))
		case Keyword:
			if !instrNames[t.Text] {
				switch t.Text {
				case "end", "else", "catch", "catch_all", "delegate":
					return list
				}
				c.fail(t, "unknown operator %s", t.Text)
			}
			list = append(list, c.flat(f))
		default:
			return list
		}
	}
}

func (c *compiler) flat(f *funcCtx) *node {
	tok := c.next()
	switch tok.Text {
	case "block", "loop", "if", "try_table", "try":
		return c.flatBlock(f, tok)
	}
	return c.plain(f, tok)
}

var blockOps = map[string]byte{"block": 0x02, "loop": 0x03, "if": 0x04, "try": 0x06, "try_table": 0x1f}

func (c *compiler) blockLabel() string {
	if c.peek().Kind == ID {
		return c.next().Text
	}
	return ""
}

func (c *compiler) blockType() blockType {
	bt := blockType{tok: c.peek()}
	if c.isParen("type") {
		c.p += 2
		bt.idx = c.index(&c.types)
		c.rparen()
		bt.explicit = true
	}
	bt.sig = c.funcType(nil)
	return bt
}

// endLabel parses the optional label after end or else, which must repeat
// the block's.
func (c *compiler) endLabel(label string) {
	if t := c.peek(); t.Kind == ID {
		c.p++
		if t.Text != label {
			c.fail(t, "mismatching label")
		}
	}
}

func (c *compiler) flatBlock(f *funcCtx, tok Token) *node {
	n := &node{kind: nBlock, tok: tok, op: blockOps[tok.Text]}
	label := c.blockLabel()
	n.bt = c.blockType()
	switch tok.Text {
	case "block", "loop":
		f.push(label)
		n.body = c.instrs(f)
		f.pop()
	case "if":
		f.push(label)
		n.body = c.instrs(f)
		if c.isKw(c.p, "else") {
			c.p++
			c.endLabel(label)
			n.els = c.instrs(f)
		}
		f.pop()
	case "try_table":
		n.catches = c.catchClauses(f)
		f.push(label)
		n.body = c.instrs(f)
		f.pop()
	case "try":
		f.push(label)
		n.body = c.instrs(f)
		sawAll := false
		for !sawAll && (c.isKw(c.p, "catch") || c.isKw(c.p, "catch_all")) {
			lc := legacyCatch{all: c.next().Text == "catch_all"}
			if lc.all {
				sawAll = true
			} else {
				lc.tag = c.index(&c.tags)
			}
			lc.body = c.instrs(f)
			n.legacy = append(n.legacy, lc)
		}
		f.pop()
		if len(n.legacy) == 0 && c.isKw(c.p, "delegate") {
			c.p++
			n.delegate = true
			n.target = c.label(f)
			return n
		}
	}
	c.expectKw("end")
	c.endLabel(label)
	return n
}

// catchClauses parses try_table's catch clauses, whose labels are outside
// the try_table.
func (c *compiler) catchClauses(f *funcCtx) []byte {
	var b []byte
	n := uint32(0)
	for {
		switch {
		case c.isParen("catch"):
			c.p += 2
			b = append(b, 0)
			b = appendU32(b, c.index(&c.tags))
			b = appendU32(b, c.label(f))
		case c.isParen("catch_ref"):
			c.p += 2
			b = append(b, 1)
			b = appendU32(b, c.index(&c.tags))
			b = appendU32(b, c.label(f))
		case c.isParen("catch_all"):
			c.p += 2
			b = append(b, 2)
			b = appendU32(b, c.label(f))
		case c.isParen("catch_all_ref"):
			c.p += 2
			b = append(b, 3)
			b = appendU32(b, c.label(f))
		default:
			return append(appendU32(nil, n), b...)
		}
		c.rparen()
		n++
	}
}

// folded parses a folded instruction, (op immediates operand*), or a
// folded block.
func (c *compiler) folded(f *funcCtx) *node {
	c.lparen()
	tok := c.next()
	if tok.Kind != Keyword || !instrNames[tok.Text] {
		c.unexpected(tok)
	}
	var n *node
	switch tok.Text {
	case "block", "loop":
		n = &node{kind: nBlock, tok: tok, op: blockOps[tok.Text]}
		label := c.blockLabel()
		n.bt = c.blockType()
		f.push(label)
		n.body = c.instrs(f)
		f.pop()
	case "if":
		n = &node{kind: nBlock, tok: tok, op: 0x04}
		label := c.blockLabel()
		n.bt = c.blockType()
		for c.peek().Kind == LParen && !c.isKw(c.p+1, "then") {
			n.operands = append(n.operands, c.folded(f))
		}
		f.push(label)
		c.expectParenKw("then")
		n.body = c.instrs(f)
		c.rparen()
		if c.isParen("else") {
			c.p += 2
			n.els = c.instrs(f)
			c.rparen()
		}
		f.pop()
	case "try_table":
		n = &node{kind: nBlock, tok: tok, op: 0x1f}
		label := c.blockLabel()
		n.bt = c.blockType()
		n.catches = c.catchClauses(f)
		f.push(label)
		n.body = c.instrs(f)
		f.pop()
	case "try":
		n = &node{kind: nBlock, tok: tok, op: 0x06}
		label := c.blockLabel()
		n.bt = c.blockType()
		f.push(label)
		c.expectParenKw("do")
		n.body = c.instrs(f)
		c.rparen()
		for c.isParen("catch") {
			c.p += 2
			lc := legacyCatch{tag: c.index(&c.tags)}
			lc.body = c.instrs(f)
			c.rparen()
			n.legacy = append(n.legacy, lc)
		}
		if c.isParen("catch_all") {
			c.p += 2
			lc := legacyCatch{all: true}
			lc.body = c.instrs(f)
			c.rparen()
			n.legacy = append(n.legacy, lc)
		}
		f.pop()
		if len(n.legacy) == 0 && c.isParen("delegate") {
			c.p += 2
			n.delegate = true
			n.target = c.label(f)
			c.rparen()
		}
	default:
		n = c.plain(f, tok)
		for c.peek().Kind == LParen {
			n.operands = append(n.operands, c.folded(f))
		}
	}
	c.rparen()
	n.folded = true
	return n
}

func (c *compiler) laneIdx() byte {
	t := c.next()
	if t.Kind != Nat {
		c.unexpected(t)
	}
	v, ok := parseUnsigned(t.Text, 8)
	if !ok {
		c.fail(t, "i8 constant out of range")
	}
	return byte(v)
}

func (c *compiler) isMemargKw(prefix string) bool {
	t := c.peek()
	return t.Kind == Keyword && strings.HasPrefix(t.Text, prefix)
}

// memargRest parses offset=? align=? and encodes the memory argument.
func (c *compiler) memargRest(b []byte, mem, natural uint32) []byte {
	off := uint64(0)
	if c.isMemargKw("offset=") {
		t := c.next()
		v, ok := ParseNat64(t.Text[len("offset="):])
		if !ok {
			c.fail(t, "i64 constant out of range")
		}
		off = v
	}
	align := natural
	if c.isMemargKw("align=") {
		t := c.next()
		v, ok := ParseNat64(t.Text[len("align="):])
		if !ok {
			c.fail(t, "i64 constant out of range")
		}
		l, ok := log2(v)
		if !ok {
			c.fail(t, "alignment must be a power of two")
		}
		align = l
	}
	if mem != 0 {
		b = appendU32(b, align|0x40)
		b = appendU32(b, mem)
	} else {
		b = appendU32(b, align)
	}
	return appendU64(b, off)
}

// memOpNode parses a memory instruction's immediates.
func (c *compiler) memOpNode(tok Token, m memOp) *node {
	b := m.op.encode(nil)
	mem := uint32(0)
	if m.lane {
		t := c.peek()
		next := c.at(c.p + 1)
		if t.Kind == ID || t.Kind == Nat && (next.Kind == Nat ||
			next.Kind == Keyword && (strings.HasPrefix(next.Text, "offset=") || strings.HasPrefix(next.Text, "align="))) {
			mem = c.index(&c.mems)
		}
		b = c.memargRest(b, mem, m.align)
		b = append(b, c.laneIdx())
	} else {
		if c.isIndex() {
			mem = c.index(&c.mems)
		}
		b = c.memargRest(b, mem, m.align)
	}
	return &node{tok: tok, enc: b}
}

func (c *compiler) optIndex(s *space) uint32 {
	if c.isIndex() {
		return c.index(s)
	}
	return 0
}

// plain parses a non-block instruction's immediates.
func (c *compiler) plain(f *funcCtx, tok Token) *node {
	name := tok.Text
	if o, ok := simpleOps[name]; ok {
		b := o.encode(nil)
		if o.kind == opLane {
			b = append(b, c.laneIdx())
		}
		return &node{tok: tok, enc: b}
	}
	if m, ok := memOps[name]; ok {
		return c.memOpNode(tok, m)
	}
	n := &node{tok: tok}
	var b []byte
	switch name {
	case "unreachable":
		b = []byte{0x00}
	case "nop":
		b = []byte{0x01}
	case "drop":
		b = []byte{0x1a}
	case "return":
		b = []byte{0x0f}
	case "throw_ref":
		b = []byte{0x0a}
	case "ref.is_null":
		b = []byte{0xd1}
	case "ref.as_non_null":
		b = []byte{0xd4}
	case "ref.eq":
		b = []byte{0xd3}
	case "ref.i31":
		b = []byte{0xfb, 0x1c}
	case "array.len":
		b = []byte{0xfb, 0x0f}
	case "atomic.fence":
		b = []byte{0xfe, 0x03, 0x00}
	case "select":
		var ts []valType
		typed := false
		for c.isParen("result") {
			c.p += 2
			ts = append(ts, c.valTypes()...)
			c.rparen()
			typed = true
		}
		if typed {
			b = appendValTypes([]byte{0x1c}, ts)
		} else {
			b = []byte{0x1b}
		}
	case "br", "br_if", "br_on_null", "br_on_non_null", "rethrow":
		code := map[string]byte{"br": 0x0c, "br_if": 0x0d, "br_on_null": 0xd5, "br_on_non_null": 0xd6, "rethrow": 0x09}[name]
		b = appendU32([]byte{code}, c.label(f))
	case "br_table":
		ls := []uint32{c.label(f)}
		for c.isIndex() {
			ls = append(ls, c.label(f))
		}
		b = appendU32([]byte{0x0e}, uint32(len(ls)-1))
		for _, l := range ls {
			b = appendU32(b, l)
		}
	case "br_on_cast", "br_on_cast_fail":
		l := c.label(f)
		t1 := c.refType()
		t2 := c.refType()
		flags := byte(0)
		if t1.code == codeRefNull {
			flags |= 1
		}
		if t2.code == codeRefNull {
			flags |= 2
		}
		code := byte(0x18)
		if name == "br_on_cast_fail" {
			code = 0x19
		}
		b = appendU32([]byte{0xfb, code, flags}, l)
		b = appendHeap(b, t1.heap)
		b = appendHeap(b, t2.heap)
	case "call", "return_call":
		code := byte(0x10)
		if name == "return_call" {
			code = 0x12
		}
		b = appendU32([]byte{code}, c.index(&c.funcs))
	case "ref.func":
		x := c.index(&c.funcs)
		n.isRefFunc, n.funcIdx = true, x
		b = appendU32([]byte{0xd2}, x)
	case "call_ref", "return_call_ref":
		code := byte(0x14)
		if name == "return_call_ref" {
			code = 0x15
		}
		b = appendU32([]byte{code}, c.index(&c.types))
	case "call_indirect", "return_call_indirect":
		n.kind = nCallIndirect
		n.ret = name == "return_call_indirect"
		n.table = c.optIndex(&c.tables)
		n.bt = c.blockType()
		return n
	case "throw":
		b = appendU32([]byte{0x08}, c.index(&c.tags))
	case "local.get", "local.set", "local.tee":
		code := map[string]byte{"local.get": 0x20, "local.set": 0x21, "local.tee": 0x22}[name]
		b = appendU32([]byte{code}, c.local(f))
	case "global.get", "global.set":
		code := byte(0x23)
		if name == "global.set" {
			code = 0x24
		}
		b = appendU32([]byte{code}, c.index(&c.globals))
	case "table.get":
		b = appendU32([]byte{0x25}, c.optIndex(&c.tables))
	case "table.set":
		b = appendU32([]byte{0x26}, c.optIndex(&c.tables))
	case "table.size":
		b = appendU32([]byte{0xfc, 16}, c.optIndex(&c.tables))
	case "table.grow":
		b = appendU32([]byte{0xfc, 15}, c.optIndex(&c.tables))
	case "table.fill":
		b = appendU32([]byte{0xfc, 17}, c.optIndex(&c.tables))
	case "table.copy":
		var x, y uint32
		if c.isIndex() {
			x = c.index(&c.tables)
			y = c.index(&c.tables)
		}
		b = appendU32(appendU32([]byte{0xfc, 14}, x), y)
	case "table.init":
		var x, y uint32
		if k := c.at(c.p + 1).Kind; k == Nat || k == ID {
			x = c.index(&c.tables)
		}
		y = c.index(&c.elems)
		b = appendU32(appendU32([]byte{0xfc, 12}, y), x)
	case "elem.drop":
		b = appendU32([]byte{0xfc, 13}, c.index(&c.elems))
	case "memory.size":
		b = appendU32([]byte{0x3f}, c.optIndex(&c.mems))
	case "memory.grow":
		b = appendU32([]byte{0x40}, c.optIndex(&c.mems))
	case "memory.fill":
		b = appendU32([]byte{0xfc, 11}, c.optIndex(&c.mems))
	case "memory.copy":
		var x, y uint32
		if c.isIndex() {
			x = c.index(&c.mems)
			y = c.index(&c.mems)
		}
		b = appendU32(appendU32([]byte{0xfc, 10}, x), y)
	case "memory.init":
		var x, y uint32
		if k := c.at(c.p + 1).Kind; k == Nat || k == ID {
			x = c.index(&c.mems)
		}
		y = c.index(&c.datas)
		b = appendU32(appendU32([]byte{0xfc, 8}, y), x)
		c.dataCount = true
	case "data.drop":
		b = appendU32([]byte{0xfc, 9}, c.index(&c.datas))
		c.dataCount = true
	case "ref.null":
		h := c.heapType()
		n.isRefNull, n.heap = true, h
		b = appendHeap([]byte{0xd0}, h)
	case "ref.test", "ref.cast":
		t := c.refType()
		code := byte(0x14)
		if name == "ref.cast" {
			code = 0x16
		}
		if t.code == codeRefNull {
			code++
		}
		b = appendHeap([]byte{0xfb, code}, t.heap)
	case "struct.new", "struct.new_default", "array.new", "array.new_default",
		"array.get", "array.get_s", "array.get_u", "array.set", "array.fill":
		code := map[string]byte{"struct.new": 0, "struct.new_default": 1, "array.new": 6,
			"array.new_default": 7, "array.get": 0x0b, "array.get_s": 0x0c, "array.get_u": 0x0d,
			"array.set": 0x0e, "array.fill": 0x10}[name]
		b = appendU32([]byte{0xfb, code}, c.index(&c.types))
	case "struct.get", "struct.get_s", "struct.get_u", "struct.set":
		code := map[string]byte{"struct.get": 2, "struct.get_s": 3, "struct.get_u": 4, "struct.set": 5}[name]
		x := c.index(&c.types)
		b = appendU32([]byte{0xfb, code}, x)
		b = appendU32(b, c.fieldIndex(x))
	case "array.new_fixed":
		x := c.index(&c.types)
		b = appendU32(appendU32([]byte{0xfb, 8}, x), c.nat32())
	case "array.new_data", "array.init_data":
		code := byte(9)
		if name == "array.init_data" {
			code = 0x12
		}
		x := c.index(&c.types)
		b = appendU32(appendU32([]byte{0xfb, code}, x), c.index(&c.datas))
		c.dataCount = true
	case "array.new_elem", "array.init_elem":
		code := byte(0x0a)
		if name == "array.init_elem" {
			code = 0x13
		}
		x := c.index(&c.types)
		b = appendU32(appendU32([]byte{0xfb, code}, x), c.index(&c.elems))
	case "array.copy":
		x := c.index(&c.types)
		b = appendU32(appendU32([]byte{0xfb, 0x11}, x), c.index(&c.types))
	case "i32.const":
		t := c.numTok()
		v, ok := ParseInt(t.Text, 32)
		if !ok {
			c.fail(t, "constant out of range")
		}
		b = appendS64([]byte{0x41}, int64(int32(uint32(v))))
	case "i64.const":
		t := c.numTok()
		v, ok := ParseInt(t.Text, 64)
		if !ok {
			c.fail(t, "constant out of range")
		}
		b = appendS64([]byte{0x42}, int64(v))
	case "f32.const":
		t := c.numTok()
		v, ok := ParseF32(t.Text)
		if !ok {
			c.fail(t, "constant out of range")
		}
		b = binary.LittleEndian.AppendUint32([]byte{0x43}, v)
	case "f64.const":
		t := c.numTok()
		v, ok := ParseF64(t.Text)
		if !ok {
			c.fail(t, "constant out of range")
		}
		b = binary.LittleEndian.AppendUint64([]byte{0x44}, v)
	case "v128.const":
		v := c.v128Lanes()
		b = append([]byte{0xfd, 0x0c}, v[:]...)
	case "i8x16.shuffle":
		b = []byte{0xfd, 0x0d}
		for i := 0; i < 16; i++ {
			if c.peek().Kind != Nat {
				c.fail(c.peek(), "wrong number of lane indices")
			}
			b = append(b, c.laneIdx())
		}
		if c.peek().Kind == Nat {
			c.fail(c.peek(), "wrong number of lane indices")
		}
	default:
		c.fail(tok, "unknown operator %s", name)
	}
	n.enc = b
	return n
}

func (c *compiler) numTok() Token {
	t := c.next()
	switch t.Kind {
	case Nat, Int, Float:
		return t
	}
	c.unexpected(t)
	return t
}

func (c *compiler) fieldIndex(x uint32) uint32 {
	t := c.peek()
	if t.Kind == ID {
		c.p++
		if x < uint32(len(c.fieldNames)) {
			if i, ok := c.fieldNames[x][t.Text]; ok {
				return i
			}
		}
		c.fail(t, "unknown field $%s", t.Text)
	}
	return c.nat32()
}

// v128Lanes parses a v128 constant's shape and lanes.
func (c *compiler) v128Lanes() [16]byte {
	shape := c.next()
	var v [16]byte
	if shape.Kind != Keyword {
		c.unexpected(shape)
	}
	lanes, width := 0, 0
	switch shape.Text {
	case "i8x16":
		lanes, width = 16, 8
	case "i16x8":
		lanes, width = 8, 16
	case "i32x4", "f32x4":
		lanes, width = 4, 32
	case "i64x2", "f64x2":
		lanes, width = 2, 64
	default:
		c.unexpected(shape)
	}
	float := shape.Text[0] == 'f'
	for i := 0; i < lanes; i++ {
		t := c.peek()
		if k := t.Kind; k != Nat && k != Int && k != Float {
			c.fail(t, "wrong number of lane literals")
		}
		c.p++
		var bits uint64
		var ok bool
		switch {
		case float && width == 32:
			var f uint32
			f, ok = ParseF32(t.Text)
			bits = uint64(f)
		case float:
			bits, ok = ParseF64(t.Text)
		default:
			bits, ok = ParseInt(t.Text, width)
		}
		if !ok {
			c.fail(t, "constant out of range")
		}
		for j := 0; j < width/8; j++ {
			v[i*width/8+j] = byte(bits >> (8 * j))
		}
	}
	if k := c.peek().Kind; k == Nat || k == Int || k == Float {
		c.fail(c.peek(), "wrong number of lane literals")
	}
	return v
}

// The implicit types an instruction sequence uses are allocated in the
// order the reference interpreter's parser allocates them, which evaluates
// the operands of its semantic actions right to left: a sequence's
// instructions last to first, a folded instruction's operands last to
// first, a block's body before the block's own type, and a folded if's
// condition first, then its else branch, its then branch and its type.

func (c *compiler) allocList(list []*node) {
	for i := len(list) - 1; i >= 0; i-- {
		c.allocNode(list[i])
	}
}

func (c *compiler) allocNode(n *node) {
	if n.folded {
		switch {
		case n.kind == nBlock && n.op == 0x04:
			for _, o := range n.operands {
				c.allocNode(o)
			}
			c.allocList(n.els)
			c.allocList(n.body)
			c.resolveBlockType(&n.bt)
		case n.kind == nBlock:
			c.allocList(n.body)
			for _, lc := range n.legacy {
				c.allocList(lc.body)
			}
			c.resolveBlockType(&n.bt)
		default:
			c.allocList(n.operands)
			if n.kind == nCallIndirect {
				c.resolveCallType(&n.bt)
			}
		}
		return
	}
	switch n.kind {
	case nBlock:
		c.allocList(n.body)
		for _, lc := range n.legacy {
			c.allocList(lc.body)
		}
		c.resolveBlockType(&n.bt)
		c.allocList(n.els)
	case nCallIndirect:
		c.resolveCallType(&n.bt)
	}
}

func (c *compiler) resolveBlockType(bt *blockType) {
	switch {
	case bt.explicit:
		c.explicitFuncType(bt.tok, bt.idx, bt.sig)
		bt.enc = appendS64(nil, int64(bt.idx))
	case len(bt.sig.params) == 0 && len(bt.sig.results) == 0:
		bt.enc = []byte{0x40}
	case len(bt.sig.params) == 0 && len(bt.sig.results) == 1:
		bt.enc = bt.sig.results[0].encode(nil)
	default:
		bt.enc = appendS64(nil, int64(c.inlineFuncType(bt.sig)))
	}
}

func (c *compiler) resolveCallType(bt *blockType) {
	if bt.explicit {
		c.explicitFuncType(bt.tok, bt.idx, bt.sig)
	} else {
		bt.idx = c.inlineFuncType(bt.sig)
	}
}

func (c *compiler) emitList(b []byte, list []*node) []byte {
	for _, n := range list {
		b = c.emit(b, n)
	}
	return b
}

func (c *compiler) emit(b []byte, n *node) []byte {
	b = c.emitList(b, n.operands)
	switch n.kind {
	case nCallIndirect:
		code := byte(0x11)
		if n.ret {
			code = 0x13
		}
		b = appendU32(append(b, code), n.bt.idx)
		return appendU32(b, n.table)
	case nBlock:
		b = append(b, n.op)
		b = append(b, n.bt.enc...)
		b = append(b, n.catches...)
		b = c.emitList(b, n.body)
		if len(n.els) > 0 {
			b = append(b, 0x05)
			b = c.emitList(b, n.els)
		}
		for _, lc := range n.legacy {
			if lc.all {
				b = append(b, 0x19)
			} else {
				b = appendU32(append(b, 0x07), lc.tag)
			}
			b = c.emitList(b, lc.body)
		}
		if n.delegate {
			return appendU32(append(b, 0x18), n.target)
		}
		return append(b, 0x0b)
	}
	return append(b, n.enc...)
}
