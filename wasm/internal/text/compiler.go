package text

import (
	"fmt"
	"unicode/utf8"
)

// space is an index space: the number of indices bound and the identifiers
// bound to them.
type space struct {
	what  string
	names map[string]uint32
	count uint32
	// byIndex keeps each named index's identifier, for the name section.
	byIndex map[uint32]string
}

func newSpace(what string) space {
	return space{what: what, names: map[string]uint32{}, byIndex: map[uint32]string{}}
}

// bind binds the next index, to id's name unless id is not an identifier.
func (s *space) bind(c *compiler, id Token) uint32 {
	x := s.count
	if s.count == ^uint32(0) {
		c.fail(id, "too many %s bindings", s.what)
	}
	s.count++
	if id.Kind == ID {
		if _, dup := s.names[id.Text]; dup {
			c.fail(id, "duplicate %s $%s", s.what, id.Text)
		}
		s.names[id.Text] = x
		s.byIndex[x] = id.Text
	}
	return x
}

type bailout struct{ err *Error }

// compiler compiles one module. It keeps the whole token stream and works
// on absolute token indices; p is the cursor.
type compiler struct {
	toks   []Token
	annots []Annot
	match  map[int]int // the matching ')' of each '(' in the module
	p      int

	types, funcs, tables, mems, globals, tags, elems, datas space

	defTypes   []defType
	recGroups  [][]*subType
	fieldNames []map[string]uint32

	// Module contents, in binary form.
	imports    [][]byte
	funcTypes  []uint32 // defined functions' type indices
	funcLocals []*localNames
	code       [][]byte
	tableSecs  [][]byte
	memSecs    [][]byte
	globalSecs [][]byte
	tagSecs    [][]byte
	exports    [][]byte
	start      *uint32
	elemSecs   [][]byte
	dataSecs   [][]byte
	dataCount  bool // whether code refers to data segments

	moduleName      *string
	moduleNameAnnot bool
	nameAnnots      map[uint32]string // function index -> @name
	tagAnnots       map[uint32]string
	customs         []custom
	exportNames     map[string]bool
}

// localNames are a function's local identifiers, by local index.
type localNames struct {
	fn    uint32
	names map[uint32]string
}

func (c *compiler) fail(t Token, format string, args ...any) {
	panic(bailout{&Error{Line: t.Line, Col: t.Col, Msg: fmt.Sprintf(format, args...)}})
}

func (c *compiler) unexpected(t Token) {
	switch t.Kind {
	case EOF:
		c.fail(t, "unexpected end of input")
	case Keyword:
		c.fail(t, "unexpected token %s", t.Text)
	}
	c.fail(t, "unexpected token")
}

func (c *compiler) peek() Token { return c.toks[c.p] }

func (c *compiler) at(i int) Token {
	if i >= len(c.toks) {
		return c.toks[len(c.toks)-1]
	}
	return c.toks[i]
}

func (c *compiler) next() Token {
	t := c.toks[c.p]
	if t.Kind != EOF {
		c.p++
	}
	return t
}

func (c *compiler) isKw(i int, kw string) bool {
	t := c.at(i)
	return t.Kind == Keyword && t.Text == kw
}

// isParen reports whether the cursor is at "(kw".
func (c *compiler) isParen(kw string) bool {
	return c.peek().Kind == LParen && c.isKw(c.p+1, kw)
}

func (c *compiler) lparen() {
	if t := c.next(); t.Kind != LParen {
		c.unexpected(t)
	}
}

func (c *compiler) rparen() {
	if t := c.next(); t.Kind != RParen {
		c.unexpected(t)
	}
}

func (c *compiler) expectKw(kw string) {
	if t := c.next(); t.Kind != Keyword || t.Text != kw {
		c.unexpected(t)
	}
}

func (c *compiler) expectParenKw(kw string) {
	c.lparen()
	c.expectKw(kw)
}

// index parses an index into space s: a number or an identifier bound in it.
func (c *compiler) index(s *space) uint32 {
	t := c.next()
	switch t.Kind {
	case Nat:
		v, ok := ParseNat32(t.Text)
		if !ok {
			c.fail(t, "i32 constant out of range")
		}
		return v
	case ID:
		x, ok := s.names[t.Text]
		if !ok {
			c.fail(t, "unknown %s $%s", s.what, t.Text)
		}
		return x
	}
	c.unexpected(t)
	return 0
}

func (c *compiler) isIndex() bool {
	k := c.peek().Kind
	return k == Nat || k == ID
}

func (c *compiler) nat32() uint32 {
	t := c.next()
	if t.Kind != Nat {
		c.unexpected(t)
	}
	v, ok := ParseNat32(t.Text)
	if !ok {
		c.fail(t, "i32 constant out of range")
	}
	return v
}

func (c *compiler) nat64() uint64 {
	t := c.next()
	if t.Kind != Nat {
		c.unexpected(t)
	}
	v, ok := ParseNat64(t.Text)
	if !ok {
		c.fail(t, "i64 constant out of range")
	}
	return v
}

// name parses a string that must be valid UTF-8.
func (c *compiler) name() string {
	t := c.next()
	if t.Kind != String {
		c.unexpected(t)
	}
	if !utf8.ValidString(t.Text) {
		c.fail(t, "malformed UTF-8 encoding")
	}
	return t.Text
}

// computeMatch pairs the parentheses of toks[lo:hi].
func (c *compiler) computeMatch(lo, hi int) {
	c.match = map[int]int{}
	var stack []int
	for i := lo; i < hi; i++ {
		switch c.toks[i].Kind {
		case LParen:
			stack = append(stack, i)
		case RParen:
			if len(stack) == 0 {
				c.fail(c.toks[i], "unexpected token")
			}
			c.match[stack[len(stack)-1]] = i
			stack = stack[:len(stack)-1]
		}
	}
	if len(stack) > 0 {
		c.fail(c.toks[hi], "unexpected end of input")
	}
}

// Compile compiles a module given as text: either (module ...) or the
// module's fields alone.
func Compile(src []byte) ([]byte, error) {
	toks, annots, err := Lex(src)
	if err != nil {
		return nil, err
	}
	if len(toks) >= 3 && toks[0].Kind == LParen && toks[1].Kind == Keyword && toks[1].Text == "module" {
		bin, end, err := CompileModule(toks, annots, 0)
		if err != nil {
			return nil, err
		}
		if toks[end].Kind != EOF {
			return nil, &Error{Line: toks[end].Line, Col: toks[end].Col, Msg: "unexpected token"}
		}
		return bin, nil
	}
	return compileFields(toks, annots, 0, len(toks)-1, true, nil)
}

// CompileModule compiles the module (module $id? field*) whose "(" is
// toks[start]. It returns the binary and the index of the token after the
// module's closing parenthesis.
func CompileModule(toks []Token, annots []Annot, start int) ([]byte, int, error) {
	end, err := closing(toks, start)
	if err != nil {
		return nil, 0, err
	}
	lo := start + 2
	var name *Token
	if toks[lo].Kind == ID {
		name = &toks[lo]
		lo++
	}
	bin, err := compileFields(toks, annots, lo, end, false, name)
	return bin, end + 1, err
}

// closing returns the index of the parenthesis that closes toks[start].
func closing(toks []Token, start int) (int, error) {
	depth := 0
	for i := start; i < len(toks); i++ {
		switch toks[i].Kind {
		case LParen:
			depth++
		case RParen:
			depth--
			if depth == 0 {
				return i, nil
			}
		case EOF:
			return 0, &Error{Line: toks[i].Line, Col: toks[i].Col, Msg: "unexpected end of input"}
		}
	}
	return 0, &Error{Msg: "unexpected end of input"}
}

// CompileFields compiles the module whose fields are toks[lo:hi], where
// toks[hi] is the module's closing parenthesis and name, if not nil, its
// identifier. A script's (module definition ...) uses it.
func CompileFields(toks []Token, annots []Annot, lo, hi int, name *Token) ([]byte, error) {
	return compileFields(toks, annots, lo, hi, false, name)
}

// compileFields compiles the module whose fields are toks[lo:hi]. inline
// says there is no (module ...) around them, so that every annotation in
// the token stream is the module's.
func compileFields(toks []Token, annots []Annot, lo, hi int, inline bool, name *Token) (bin []byte, err error) {
	c := &compiler{
		toks:  toks,
		types: newSpace("type"), funcs: newSpace("function"), tables: newSpace("table"),
		mems: newSpace("memory"), globals: newSpace("global"), tags: newSpace("tag"),
		elems: newSpace("elem segment"), datas: newSpace("data segment"),
	}
	defer func() {
		if r := recover(); r != nil {
			b, ok := r.(bailout)
			if !ok {
				panic(r)
			}
			bin, err = nil, b.err
		}
	}()
	// The module's annotations are those among its tokens.
	from := lo
	if !inline {
		from = lo - 1 // just after "(module", before or after the name
		for from > 0 && toks[from].Kind != Keyword {
			from--
		}
	}
	for _, a := range annots {
		if inline || a.At > from && a.At <= hi {
			c.annots = append(c.annots, a)
		}
	}
	if name != nil {
		n := name.Text
		c.moduleName = &n
	}
	c.computeMatch(lo, hi)
	fields := c.scanFields(lo, hi)
	c.annotations(fields, from, hi, inline)
	c.defineTypes(fields)
	for i := range fields {
		c.field(&fields[i])
	}
	return c.encodeModule(), nil
}

// field is a module field: its keyword and the tokens of its "(" and ")".
type field struct {
	kind       string
	start, end int
	idx        uint32 // the index bound, for an indexed field
	isImport   bool
	inline     bool   // a memory's inline data, or a table's inline elements
	inlineIdx  uint32 // the index of the inline segment
	nameSlot   int    // the token index right after the field's keyword and identifier
}

// scanFields binds every field's indices, in order, before any field is
// compiled, so that a field can refer to those after it. A definition of a
// function, table, memory, global or tag may not precede an import.
func (c *compiler) scanFields(lo, hi int) []field {
	var fields []field
	sawDef := ""
	for p := lo; p < hi; {
		t := c.toks[p]
		if t.Kind != LParen || c.at(p+1).Kind != Keyword {
			c.unexpected(t)
		}
		f := field{kind: c.toks[p+1].Text, start: p, end: c.match[p]}
		q := p + 2
		id := Token{}
		if c.at(q).Kind == ID {
			id = c.toks[q]
			q++
		}
		f.nameSlot = q
		switch f.kind {
		case "type":
			f.idx = c.types.bind(c, id)
		case "rec":
			for q := p + 2; q < f.end; q = c.match[q] + 1 {
				if c.toks[q].Kind != LParen {
					c.unexpected(c.toks[q])
				}
				tid := Token{}
				if c.at(q+2).Kind == ID {
					tid = c.toks[q+2]
				}
				c.types.bind(c, tid)
			}
		case "func", "table", "memory", "global", "tag":
			for c.toks[q].Kind == LParen && c.isKw(q+1, "export") {
				q = c.match[q] + 1
			}
			f.isImport = c.toks[q].Kind == LParen && c.isKw(q+1, "import")
			f.idx = c.spaceOf(f.kind).bind(c, id)
			if !f.isImport {
				if f.kind == "memory" {
					if c.isKw(q, "i32") || c.isKw(q, "i64") {
						q++
					}
					if c.toks[q].Kind == LParen && c.isKw(q+1, "data") {
						f.inline = true
						f.inlineIdx = c.datas.bind(c, Token{})
					}
				}
				if f.kind == "table" {
					if c.isKw(q, "i32") || c.isKw(q, "i64") {
						q++
					}
					if c.toks[q].Kind == Keyword {
						q++
					} else if c.toks[q].Kind == LParen && c.isKw(q+1, "ref") {
						q = c.match[q] + 1
					}
					if c.toks[q].Kind == LParen && c.isKw(q+1, "elem") {
						f.inline = true
						f.inlineIdx = c.elems.bind(c, Token{})
					}
				}
			}
		case "import":
			// (import "m" "n" (kind $id? ...))
			q = p + 4
			if c.at(q).Kind == LParen && c.at(q+1).Kind == Keyword {
				if s := c.spaceOf(c.toks[q+1].Text); s != nil {
					did := Token{}
					if c.at(q+2).Kind == ID {
						did = c.toks[q+2]
					}
					f.idx = s.bind(c, did)
				}
			}
			f.isImport = true
		case "data":
			f.idx = c.datas.bind(c, id)
		case "elem":
			f.idx = c.elems.bind(c, id)
		case "export", "start":
		default:
			c.unexpected(c.toks[p+1])
		}
		switch f.kind {
		case "func", "table", "memory", "global", "tag", "import":
			if f.isImport && sawDef != "" {
				c.fail(t, "import after %s definition", sawDef)
			}
			if !f.isImport {
				sawDef = map[string]string{"func": "function"}[f.kind]
				if sawDef == "" {
					sawDef = f.kind
				}
			}
		}
		fields = append(fields, f)
		p = f.end + 1
	}
	return fields
}

func (c *compiler) spaceOf(kind string) *space {
	switch kind {
	case "func":
		return &c.funcs
	case "table":
		return &c.tables
	case "memory":
		return &c.mems
	case "global":
		return &c.globals
	case "tag":
		return &c.tags
	}
	return nil
}

// defineTypes defines the explicit types, in order, before the other fields
// are compiled: the implicit types those add follow every explicit one.
func (c *compiler) defineTypes(fields []field) {
	x := uint32(0)
	for _, f := range fields {
		switch f.kind {
		case "type":
			c.p = f.start
			st := c.typeDef(x)
			c.recGroups = append(c.recGroups, []*subType{st})
			c.defineType(st, 1)
			x++
		case "rec":
			c.p = f.start + 2
			var group []*subType
			for c.peek().Kind != RParen {
				group = append(group, c.typeDef(x))
				x++
			}
			c.rparen()
			c.recGroups = append(c.recGroups, group)
			for _, st := range group {
				c.defineType(st, len(group))
			}
		}
	}
}

// field compiles a field, after every index is bound and every explicit type
// defined. Fields are compiled in order.
func (c *compiler) field(f *field) {
	c.p = f.start + 2
	switch f.kind {
	case "type", "rec":
		return
	case "func":
		c.funcField(f)
	case "table":
		c.tableField(f)
	case "memory":
		c.memoryField(f)
	case "global":
		c.globalField(f)
	case "tag":
		c.tagField(f)
	case "import":
		c.importField(f)
	case "export":
		c.exportField()
	case "start":
		x := c.index(&c.funcs)
		c.rparen()
		if c.start != nil {
			c.fail(c.toks[f.start], "multiple start sections")
		}
		c.start = &x
	case "data":
		c.dataField(f)
	case "elem":
		c.elemField(f)
	}
	if c.p != f.end+1 {
		c.unexpected(c.toks[c.p])
	}
}

// fieldHead skips a field's identifier and parses its inline exports, of
// the given external kind, and returns whether an inline import follows,
// with its module and item names.
func (c *compiler) fieldHead(f *field, kind byte) (imp bool, mod, item string) {
	if c.peek().Kind == ID {
		c.p++
	}
	for c.isParen("export") {
		c.p += 2
		c.addExport(c.name(), kind, f.idx)
		c.rparen()
	}
	if c.isParen("import") {
		c.p += 2
		mod, item = c.name(), c.name()
		c.rparen()
		return true, mod, item
	}
	return false, "", ""
}

func (c *compiler) addExport(name string, kind byte, x uint32) {
	b := appendName(nil, name)
	b = append(b, kind)
	b = appendU32(b, x)
	c.exports = append(c.exports, b)
}

func (c *compiler) addImport(mod, item string, desc []byte) {
	b := appendName(nil, mod)
	b = appendName(b, item)
	c.imports = append(c.imports, append(b, desc...))
}

// typeUse parses a function's type: (type x)? followed by its inline
// signature, and returns its type index. names, if not nil, collects the
// parameters' identifiers.
func (c *compiler) typeUse(names *sigNames) (x uint32, explicit bool, s funcSig) {
	tok := c.peek()
	if c.isParen("type") {
		c.p += 2
		x = c.index(&c.types)
		c.rparen()
		explicit = true
	}
	s = c.funcType(names)
	if explicit {
		c.explicitFuncType(tok, x, s)
	} else {
		x = c.inlineFuncType(s)
	}
	return x, explicit, s
}

func (c *compiler) funcField(f *field) {
	imp, mod, item := c.fieldHead(f, 0)
	if imp {
		x, _, _ := c.typeUse(&sigNames{})
		c.rparen()
		c.addImport(mod, item, appendU32([]byte{0}, x))
		return
	}
	headTok := c.peek()
	names := &sigNames{}
	x, explicit, s := c.typeUse(names)
	c.funcTypes = append(c.funcTypes, x)
	fc := &funcCtx{locals: newSpace("local"), labels: []string{""}}
	// The parameters.
	nparams := -1
	if explicit && s.empty() {
		if x < uint32(len(c.defTypes)) && c.defTypes[x].st.kind == 0x60 {
			nparams = len(c.defTypes[x].st.params)
		}
		if nparams >= 0 {
			fc.locals.count = uint32(nparams)
		}
	} else {
		for _, id := range names.names {
			fc.locals.bind(c, id)
		}
	}
	// The locals.
	var locals []valType
	for c.isParen("local") {
		c.p += 2
		if c.peek().Kind == ID {
			id := c.next()
			if nparams < 0 && explicit && s.empty() {
				c.funcTypeOf(headTok, x) // fails
			}
			fc.locals.bind(c, id)
			locals = append(locals, c.valType())
		} else {
			ts := c.valTypes()
			for range ts {
				fc.locals.bind(c, Token{})
			}
			locals = append(locals, ts...)
		}
		c.rparen()
	}
	body := c.instrs(fc)
	c.rparen()
	c.allocList(body)
	if len(fc.locals.byIndex) > 0 {
		c.funcLocals = append(c.funcLocals, &localNames{fn: f.idx, names: fc.locals.byIndex})
	}
	// The body: its locals, run-length encoded, then its instructions.
	var b []byte
	var groups [][2]int // start, count
	for i := range locals {
		if i > 0 && locals[i] == locals[i-1] {
			groups[len(groups)-1][1]++
		} else {
			groups = append(groups, [2]int{i, 1})
		}
	}
	b = appendU32(b, uint32(len(groups)))
	for _, g := range groups {
		b = appendU32(b, uint32(g[1]))
		b = locals[g[0]].encode(b)
	}
	b = c.emitList(b, body)
	b = append(b, 0x0b)
	c.code = append(c.code, b)
}

func (c *compiler) tableField(f *field) {
	imp, mod, item := c.fieldHead(f, 1)
	if imp {
		desc := c.tableType(nil)
		c.rparen()
		c.addImport(mod, item, append([]byte{1}, desc...))
		return
	}
	if f.inline {
		at64 := c.addrType()
		rt := c.refType()
		c.expectParenKw("elem")
		items := c.elemItems(itemsEither)
		c.rparen()
		c.rparen()
		n := uint64(len(items))
		b := rt.encode(nil)
		b = appendLimits(b, at64, n, &n, false, 0)
		c.tableSecs = append(c.tableSecs, b)
		c.elemSecs = append(c.elemSecs, c.encodeElem(rt, items, elemActive, f.idx, constZero(at64)))
		return
	}
	var rt valType
	desc := c.tableType(&rt)
	if c.peek().Kind == RParen {
		c.rparen()
		c.tableSecs = append(c.tableSecs, desc)
		return
	}
	init := c.constExpr(true)
	c.rparen()
	if len(init) == 1 && init[0].isRefNull && init[0].heap == rt.heap && len(init[0].operands) == 0 {
		c.tableSecs = append(c.tableSecs, desc)
		return
	}
	b := append([]byte{0x40, 0x00}, desc...)
	b = c.emitList(b, init)
	c.tableSecs = append(c.tableSecs, append(b, 0x0b))
}

func (c *compiler) addrType() bool {
	switch {
	case c.isKw(c.p, "i64"):
		c.p++
		return true
	case c.isKw(c.p, "i32"):
		c.p++
	}
	return false
}

// tableType parses addrtype? limits reftype.
func (c *compiler) tableType(rt *valType) []byte {
	at64 := c.addrType()
	min, max := c.limits()
	t := c.refType()
	if rt != nil {
		*rt = t
	}
	b := t.encode(nil)
	return appendLimits(b, at64, min, max, false, 0)
}

func (c *compiler) limits() (uint64, *uint64) {
	min := c.nat64()
	if c.peek().Kind == Nat {
		max := c.nat64()
		return min, &max
	}
	return min, nil
}

func appendLimits(b []byte, at64 bool, min uint64, max *uint64, shared bool, pageLog2 int) []byte {
	flags := byte(0)
	if max != nil {
		flags |= 1
	}
	if shared {
		flags |= 2
	}
	if at64 {
		flags |= 4
	}
	if pageLog2 >= 0 && pageLog2 != 0 {
		flags |= 8
	}
	b = append(b, flags)
	b = appendU64(b, min)
	if max != nil {
		b = appendU64(b, *max)
	}
	if flags&8 != 0 {
		b = appendU32(b, uint32(pageLog2-1))
	}
	return b
}

// memType parses addrtype? limits shared? (pagesize n)?.
func (c *compiler) memType() []byte {
	at64 := c.addrType()
	min, max := c.limits()
	shared := false
	if c.isKw(c.p, "shared") {
		c.p++
		shared = true
	}
	page := 0
	if c.isParen("pagesize") {
		c.p += 2
		tok := c.peek()
		l, ok := log2(c.nat64())
		if !ok {
			c.fail(tok, "invalid custom page size")
		}
		page = int(l) + 1
		c.rparen()
	}
	return appendLimits(nil, at64, min, max, shared, page)
}

func (c *compiler) memoryField(f *field) {
	imp, mod, item := c.fieldHead(f, 2)
	if imp {
		desc := c.memType()
		c.rparen()
		c.addImport(mod, item, append([]byte{2}, desc...))
		return
	}
	if f.inline {
		at64 := c.addrType()
		c.expectParenKw("data")
		var data []byte
		for c.peek().Kind == String {
			data = append(data, c.next().Text...)
		}
		c.rparen()
		c.rparen()
		n := (uint64(len(data)) + 65535) / 65536
		c.memSecs = append(c.memSecs, appendLimits(nil, at64, n, &n, false, 0))
		c.dataSecs = append(c.dataSecs, encodeData(data, true, f.idx, constZero(at64)))
		return
	}
	desc := c.memType()
	c.rparen()
	c.memSecs = append(c.memSecs, desc)
}

func constZero(at64 bool) []byte {
	if at64 {
		return []byte{0x42, 0x00, 0x0b}
	}
	return []byte{0x41, 0x00, 0x0b}
}

func (c *compiler) globalType() []byte {
	if c.isParen("mut") {
		c.p += 2
		t := c.valType()
		c.rparen()
		return append(t.encode(nil), 1)
	}
	return append(c.valType().encode(nil), 0)
}

func (c *compiler) globalField(f *field) {
	imp, mod, item := c.fieldHead(f, 3)
	if imp {
		desc := c.globalType()
		c.rparen()
		c.addImport(mod, item, append([]byte{3}, desc...))
		return
	}
	b := c.globalType()
	init := c.constExpr(false)
	c.rparen()
	b = c.emitList(b, init)
	c.globalSecs = append(c.globalSecs, append(b, 0x0b))
}

func (c *compiler) tagField(f *field) {
	imp, mod, item := c.fieldHead(f, 4)
	x, _, _ := c.typeUse(&sigNames{})
	c.rparen()
	desc := appendU32([]byte{0}, x)
	if imp {
		c.addImport(mod, item, append([]byte{4}, desc...))
		return
	}
	c.tagSecs = append(c.tagSecs, desc)
}

func (c *compiler) importField(f *field) {
	mod, item := c.name(), c.name()
	c.lparen()
	kw := c.next()
	if kw.Kind == ID || kw.Kind != Keyword {
		c.unexpected(kw)
	}
	if c.peek().Kind == ID {
		c.p++
	}
	var desc []byte
	switch kw.Text {
	case "func":
		var x uint32
		if c.isParen("type") {
			c.p += 2
			x = c.index(&c.types)
			c.rparen()
		} else {
			x = c.inlineFuncType(c.funcType(&sigNames{}))
		}
		desc = appendU32([]byte{0}, x)
	case "tag":
		var x uint32
		if c.isParen("type") {
			c.p += 2
			x = c.index(&c.types)
			c.rparen()
		} else {
			x = c.inlineFuncType(c.funcType(&sigNames{}))
		}
		desc = appendU32([]byte{4, 0}, x)
	case "global":
		desc = append([]byte{3}, c.globalType()...)
	case "memory":
		desc = append([]byte{2}, c.memType()...)
	case "table":
		desc = append([]byte{1}, c.tableType(nil)...)
	default:
		c.unexpected(kw)
	}
	c.rparen()
	c.rparen()
	c.addImport(mod, item, desc)
}

func (c *compiler) exportField() {
	name := c.name()
	c.lparen()
	kw := c.next()
	var kind byte
	var s *space
	switch {
	case kw.Kind != Keyword:
		c.unexpected(kw)
	case kw.Text == "func":
		kind, s = 0, &c.funcs
	case kw.Text == "table":
		kind, s = 1, &c.tables
	case kw.Text == "memory":
		kind, s = 2, &c.mems
	case kw.Text == "global":
		kind, s = 3, &c.globals
	case kw.Text == "tag":
		kind, s = 4, &c.tags
	default:
		c.unexpected(kw)
	}
	x := c.index(s)
	c.rparen()
	c.rparen()
	c.addExport(name, kind, x)
}

// offset parses (offset instr*) or a folded instruction, and returns the
// encoded expression.
func (c *compiler) offset() []byte {
	var e []*node
	if c.isParen("offset") {
		c.p += 2
		e = c.constExpr(false)
		c.rparen()
	} else {
		e = []*node{c.foldedConst()}
	}
	return append(c.emitList(nil, e), 0x0b)
}

// foldedConst parses a single folded instruction in a constant context.
func (c *compiler) foldedConst() *node {
	fc := &funcCtx{locals: newSpace("local")}
	if c.peek().Kind != LParen {
		c.unexpected(c.peek())
	}
	n := c.folded(fc)
	c.allocList([]*node{n})
	return n
}

// constExpr parses an instruction sequence outside a function. With one,
// it must have at least one instruction.
func (c *compiler) constExpr(one bool) []*node {
	fc := &funcCtx{locals: newSpace("local")}
	e := c.instrs(fc)
	if one && len(e) == 0 {
		c.unexpected(c.peek())
	}
	c.allocList(e)
	return e
}

func (c *compiler) dataField(f *field) {
	if c.peek().Kind == ID {
		c.p++
	}
	mem := uint32(0)
	active := false
	var off []byte
	if c.isParen("memory") {
		c.p += 2
		mem = c.index(&c.mems)
		c.rparen()
		off = c.offset()
		active = true
	} else if c.peek().Kind == LParen {
		off = c.offset()
		active = true
	}
	var data []byte
	for c.peek().Kind == String {
		data = append(data, c.next().Text...)
	}
	c.rparen()
	c.dataSecs = append(c.dataSecs, encodeData(data, active, mem, off))
}

func encodeData(data []byte, active bool, mem uint32, off []byte) []byte {
	var b []byte
	switch {
	case !active:
		b = []byte{1}
	case mem == 0:
		b = append([]byte{0}, off...)
	default:
		b = appendU32([]byte{2}, mem)
		b = append(b, off...)
	}
	b = appendU32(b, uint32(len(data)))
	return append(b, data...)
}

type elemMode uint8

const (
	elemPassive elemMode = iota
	elemActive
	elemDeclare
)

// elemItem is an element segment's item: a constant expression, encoded,
// or a function index the reference interpreter would encode as one.
type elemItem struct {
	expr    []byte
	funcIdx uint32
	isIndex bool
}

func (c *compiler) elemField(f *field) {
	if c.peek().Kind == ID {
		c.p++
	}
	mode := elemPassive
	table := uint32(0)
	var off []byte
	switch {
	case c.isKw(c.p, "declare"):
		c.p++
		mode = elemDeclare
	case c.isParen("table"):
		c.p += 2
		table = c.index(&c.tables)
		c.rparen()
		off = c.offset()
		mode = elemActive
	case c.peek().Kind == LParen && !c.isRefType():
		off = c.offset()
		mode = elemActive
		if !c.isKw(c.p, "func") && !c.isRefType() {
			// The abbreviation: offset funcidx*.
			items := c.elemItems(itemsIndices)
			c.rparen()
			c.elemSecs = append(c.elemSecs, c.encodeElem(valType{code: codeRef, heap: -0x70}, items, mode, 0, off))
			return
		}
	}
	var rt valType
	if c.isKw(c.p, "func") {
		c.p++
		rt = valType{code: codeRef, heap: -0x70}
		items := c.elemItems(itemsIndices)
		c.rparen()
		c.elemSecs = append(c.elemSecs, c.encodeElem(rt, items, mode, table, off))
		return
	}
	rt = c.refType()
	items := c.elemItems(itemsExprs)
	c.rparen()
	c.elemSecs = append(c.elemSecs, c.encodeElem(rt, items, mode, table, off))
}

const (
	itemsIndices = iota
	itemsExprs
	itemsEither // a table's inline elements: expressions if the first is one
)

// elemItems parses the items of an element list up to the closing
// parenthesis: expressions or function indices.
func (c *compiler) elemItems(kind int) []elemItem {
	var items []elemItem
	exprs := kind == itemsExprs || kind == itemsEither && c.peek().Kind == LParen
	for c.peek().Kind != RParen {
		if !exprs {
			items = append(items, elemItem{funcIdx: c.index(&c.funcs), isIndex: true})
			continue
		}
		var e []*node
		if c.isParen("item") {
			c.p += 2
			e = c.constExpr(false)
			c.rparen()
		} else {
			e = []*node{c.foldedConst()}
		}
		it := elemItem{expr: append(c.emitList(nil, e), 0x0b)}
		if len(e) == 1 && e[0].isRefFunc && len(e[0].operands) == 0 {
			it.isIndex = true
			it.funcIdx = e[0].funcIdx
		}
		items = append(items, it)
	}
	return items
}

func (c *compiler) encodeElem(rt valType, items []elemItem, mode elemMode, table uint32, off []byte) []byte {
	allIndex := true
	for _, it := range items {
		if !it.isIndex {
			allIndex = false
		}
	}
	var b []byte
	if rt == (valType{code: codeRef, heap: -0x70}) && allIndex {
		switch {
		case mode == elemPassive:
			b = []byte{1, 0}
		case mode == elemActive && table == 0:
			b = append([]byte{0}, off...)
		case mode == elemActive:
			b = appendU32([]byte{2}, table)
			b = append(b, off...)
			b = append(b, 0)
		default:
			b = []byte{3, 0}
		}
		b = appendU32(b, uint32(len(items)))
		for _, it := range items {
			b = appendU32(b, it.funcIdx)
		}
		return b
	}
	switch {
	case mode == elemPassive:
		b = rt.encode([]byte{5})
	case mode == elemActive && table == 0 && rt == (valType{code: codeRefNull, heap: -0x70}):
		b = append([]byte{4}, off...)
	case mode == elemActive:
		b = appendU32([]byte{6}, table)
		b = append(b, off...)
		b = rt.encode(b)
	default:
		b = rt.encode([]byte{7})
	}
	b = appendU32(b, uint32(len(items)))
	for _, it := range items {
		if it.expr == nil {
			it.expr = append(appendU32([]byte{0xd2}, it.funcIdx), 0x0b)
		}
		b = append(b, it.expr...)
	}
	return b
}
