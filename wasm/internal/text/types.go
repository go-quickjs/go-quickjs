package text

// valType is a value or storage type. code is the type's binary code: 0x7f
// to 0x7b for the number and vector types, 0x78 and 0x77 for the packed
// types, and 0x63 or 0x64 for a nullable or non-nullable reference, whose
// heap type is heap: a type index, or the negated code of an abstract heap
// type.
type valType struct {
	code byte
	heap int64
}

const (
	codeRefNull byte = 0x63
	codeRef     byte = 0x64
)

func (t valType) isRef() bool { return t.code == codeRefNull || t.code == codeRef }

func (t valType) encode(b []byte) []byte {
	if !t.isRef() {
		return append(b, t.code)
	}
	if t.code == codeRefNull && t.heap < 0 {
		// The shorthand of a nullable abstract reference type.
		return append(b, byte(-t.heap))
	}
	return appendHeap(append(b, t.code), t.heap)
}

func appendHeap(b []byte, h int64) []byte {
	if h < 0 {
		return append(b, byte(-h))
	}
	return appendS64(b, h)
}

// abstractHeap maps the abstract heap types' keywords to their codes.
var abstractHeap = map[string]byte{
	"any": 0x6e, "none": 0x71, "eq": 0x6d, "i31": 0x6c, "struct": 0x6b,
	"array": 0x6a, "func": 0x70, "nofunc": 0x73, "exn": 0x69, "noexn": 0x74,
	"extern": 0x6f, "noextern": 0x72,
}

// refShorthand maps the shorthand reference types to their heap types.
var refShorthand = map[string]byte{
	"anyref": 0x6e, "nullref": 0x71, "eqref": 0x6d, "i31ref": 0x6c,
	"structref": 0x6b, "arrayref": 0x6a, "funcref": 0x70, "nullfuncref": 0x73,
	"exnref": 0x69, "nullexnref": 0x74, "externref": 0x6f, "nullexternref": 0x72,
}

var numTypes = map[string]byte{
	"i32": 0x7f, "i64": 0x7e, "f32": 0x7d, "f64": 0x7c, "v128": 0x7b,
}

type fieldType struct {
	t   valType
	mut bool
}

// subType is a type definition.
type subType struct {
	final   bool
	supers  []uint32
	kind    byte // 0x60 func, 0x5f struct, 0x5e array
	params  []valType
	results []valType
	fields  []fieldType
}

// defType is a type index's definition: a subtype in a recursion group of
// the given size.
type defType struct {
	st        *subType
	groupSize int
}

func (st *subType) encode(b []byte) []byte {
	if !st.final || len(st.supers) > 0 {
		if st.final {
			b = append(b, 0x4f)
		} else {
			b = append(b, 0x50)
		}
		b = appendU32(b, uint32(len(st.supers)))
		for _, s := range st.supers {
			b = appendU32(b, s)
		}
	}
	b = append(b, st.kind)
	switch st.kind {
	case 0x60:
		b = appendValTypes(b, st.params)
		b = appendValTypes(b, st.results)
	case 0x5f:
		b = appendU32(b, uint32(len(st.fields)))
		for _, f := range st.fields {
			b = f.encode(b)
		}
	case 0x5e:
		b = st.fields[0].encode(b)
	}
	return b
}

func (f fieldType) encode(b []byte) []byte {
	b = f.t.encode(b)
	if f.mut {
		return append(b, 1)
	}
	return append(b, 0)
}

func appendValTypes(b []byte, ts []valType) []byte {
	b = appendU32(b, uint32(len(ts)))
	for _, t := range ts {
		b = t.encode(b)
	}
	return b
}

func sameTypes(a, b []valType) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// funcSig is an inline function type.
type funcSig struct {
	params, results []valType
}

func (s funcSig) empty() bool { return len(s.params) == 0 && len(s.results) == 0 }

// heapType parses a heap type: an abstract heap type's keyword or a type
// index.
func (c *compiler) heapType() int64 {
	t := c.peek()
	if t.Kind == Keyword {
		if code, ok := abstractHeap[t.Text]; ok {
			c.p++
			return -int64(code)
		}
	}
	if t.Kind == Nat || t.Kind == ID {
		return int64(c.index(&c.types))
	}
	c.unexpected(t)
	return 0
}

// isRefType reports whether a reference type starts at the cursor.
func (c *compiler) isRefType() bool {
	t := c.peek()
	if t.Kind == Keyword {
		_, ok := refShorthand[t.Text]
		return ok
	}
	return t.Kind == LParen && c.isKw(c.p+1, "ref")
}

func (c *compiler) refType() valType {
	t := c.peek()
	if t.Kind == Keyword {
		if code, ok := refShorthand[t.Text]; ok {
			c.p++
			return valType{code: codeRefNull, heap: -int64(code)}
		}
	}
	if t.Kind != LParen || !c.isKw(c.p+1, "ref") {
		c.unexpected(t)
	}
	c.p += 2
	code := codeRef
	if c.isKw(c.p, "null") {
		c.p++
		code = codeRefNull
	}
	h := c.heapType()
	c.rparen()
	return valType{code: code, heap: h}
}

func (c *compiler) isValType() bool {
	t := c.peek()
	if t.Kind == Keyword {
		if _, ok := numTypes[t.Text]; ok {
			return true
		}
	}
	return c.isRefType()
}

func (c *compiler) valType() valType {
	t := c.peek()
	if t.Kind == Keyword {
		if code, ok := numTypes[t.Text]; ok {
			c.p++
			return valType{code: code}
		}
	}
	return c.refType()
}

func (c *compiler) storageType() valType {
	switch {
	case c.isKw(c.p, "i8"):
		c.p++
		return valType{code: 0x78}
	case c.isKw(c.p, "i16"):
		c.p++
		return valType{code: 0x77}
	}
	return c.valType()
}

func (c *compiler) isFieldType() bool {
	return c.isValType() || c.isKw(c.p, "i8") || c.isKw(c.p, "i16") || c.isParen("mut")
}

func (c *compiler) fieldType() fieldType {
	if c.isParen("mut") {
		c.p += 2
		t := c.storageType()
		c.rparen()
		return fieldType{t: t, mut: true}
	}
	return fieldType{t: c.storageType()}
}

// valTypes parses value types up to the closing parenthesis.
func (c *compiler) valTypes() []valType {
	var ts []valType
	for c.peek().Kind != RParen {
		ts = append(ts, c.valType())
	}
	return ts
}

// sigNames records a function type's parameter names, for its locals.
type sigNames struct {
	names []Token // one per parameter; Kind EOF where it has none
}

// funcType parses (param ...)* (result ...)*. With names, a parameter may
// be named: (param $x t).
func (c *compiler) funcType(names *sigNames) funcSig {
	var s funcSig
	for c.isParen("param") {
		c.p += 2
		if c.peek().Kind == ID {
			if names == nil {
				c.unexpected(c.peek())
			}
			id := c.next()
			s.params = append(s.params, c.valType())
			names.names = append(names.names, id)
			c.rparen()
			continue
		}
		ts := c.valTypes()
		s.params = append(s.params, ts...)
		if names != nil {
			for range ts {
				names.names = append(names.names, Token{})
			}
		}
		c.rparen()
	}
	for c.isParen("result") {
		c.p += 2
		s.results = append(s.results, c.valTypes()...)
		c.rparen()
	}
	return s
}

// typeDef parses a (type $id? subtype) whose index is x.
func (c *compiler) typeDef(x uint32) *subType {
	c.expectParenKw("type")
	if c.peek().Kind == ID {
		c.p++
	}
	st := &subType{final: true}
	if c.isParen("sub") {
		c.p += 2
		st.final = false
		if c.isKw(c.p, "final") {
			c.p++
			st.final = true
		}
		for c.peek().Kind == Nat || c.peek().Kind == ID {
			st.supers = append(st.supers, c.index(&c.types))
		}
		c.compType(st, x)
		c.rparen()
	} else {
		c.compType(st, x)
	}
	c.rparen()
	return st
}

func (c *compiler) compType(st *subType, x uint32) {
	c.lparen()
	kw := c.next()
	switch {
	case kw.Kind == Keyword && kw.Text == "func":
		st.kind = 0x60
		s := c.funcType(&sigNames{})
		st.params, st.results = s.params, s.results
	case kw.Kind == Keyword && kw.Text == "struct":
		st.kind = 0x5f
		for c.isParen("field") {
			c.p += 2
			if c.peek().Kind == ID {
				id := c.next()
				c.bindField(x, id, uint32(len(st.fields)))
				st.fields = append(st.fields, c.fieldType())
			} else {
				for c.isFieldType() {
					st.fields = append(st.fields, c.fieldType())
				}
			}
			c.rparen()
		}
	case kw.Kind == Keyword && kw.Text == "array":
		st.kind = 0x5e
		st.fields = []fieldType{c.fieldType()}
	default:
		c.unexpected(kw)
	}
	c.rparen()
}

func (c *compiler) bindField(x uint32, id Token, i uint32) {
	for uint32(len(c.fieldNames)) <= x {
		c.fieldNames = append(c.fieldNames, nil)
	}
	m := c.fieldNames[x]
	if m == nil {
		m = map[string]uint32{}
		c.fieldNames[x] = m
	}
	if _, dup := m[id.Text]; dup {
		c.fail(id, "duplicate field $%s", id.Text)
	}
	m[id.Text] = i
}

// defineType adds a type definition to the type index space's definitions.
func (c *compiler) defineType(st *subType, groupSize int) {
	c.defTypes = append(c.defTypes, defType{st: st, groupSize: groupSize})
}

// inlineFuncType returns the index of the first type definition that is a
// final function type without supertypes alone in its recursion group with
// the signature s, adding one at the end of the type section if there is
// none.
func (c *compiler) inlineFuncType(s funcSig) uint32 {
	for i, d := range c.defTypes {
		st := d.st
		if d.groupSize == 1 && st.final && len(st.supers) == 0 && st.kind == 0x60 &&
			sameTypes(st.params, s.params) && sameTypes(st.results, s.results) {
			return uint32(i)
		}
	}
	x := c.types.bind(c, Token{})
	st := &subType{final: true, kind: 0x60, params: s.params, results: s.results}
	c.recGroups = append(c.recGroups, []*subType{st})
	c.defineType(st, 1)
	return x
}

// funcTypeOf returns the signature of type x, failing at tok if it is not a
// function type.
func (c *compiler) funcTypeOf(tok Token, x uint32) funcSig {
	if x >= uint32(len(c.defTypes)) {
		c.fail(tok, "unknown type %d", x)
	}
	st := c.defTypes[x].st
	if st.kind != 0x60 {
		c.fail(tok, "non-function type %d", x)
	}
	return funcSig{params: st.params, results: st.results}
}

// explicitFuncType checks an explicit type use's inline signature against
// the type it names.
func (c *compiler) explicitFuncType(tok Token, x uint32, s funcSig) {
	if s.empty() {
		return
	}
	t := c.funcTypeOf(tok, x)
	if !sameTypes(t.params, s.params) || !sameTypes(t.results, s.results) {
		c.fail(tok, "inline function type does not match explicit type")
	}
}
