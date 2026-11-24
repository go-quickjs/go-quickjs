package vm

import (
	"strings"
)

// JSON.parse's parser.

// jsonParser is a recursive-descent parser for JSON text.
//
// An object is not built as its keys are read, as an object made one property
// at a time would be: its values wait on scratch while the parser walks the
// shape tree, and it is made once, at its last key, in the size it needs.
// Which shape comes next is guessed, as V8's parser takes its guess from the
// object before: the last object parsed in the same place in a document says
// each key to expect (hints), and past that a shape's first transition does,
// so that a key that was expected costs a comparison of its text with a name,
// where any other is hashed. A transition out of a shape is for a key the
// shape does not have, which is what shows the keys of an object made this
// way to be different: a key given twice is met where no transition leads, and
// such an object is built by the table (parseEntries), with the rest of what
// the walk cannot do.
type jsonParser struct {
	rt  *Runtime
	src string
	pos int
	// room is how many levels more the parser may descend, which is
	// bounded: the stack it recurses on cannot grow for ever (parseJSON).
	room int
	// scratch collects the elements of the arrays and the values of the
	// objects being parsed, each above its parent's. Each is copied out at
	// exactly its own length when it closes, so growing this one buffer
	// replaces growing a slice per array. Above its length it is zero, so
	// that it holds nothing once it is empty.
	scratch []Value

	// st is what is kept from one parse to the next. root is the shape an
	// object starts from, or nil where objects are built by the table: for
	// a reviver's records, and where the runtime's objects share no layout.
	st    *jsonState
	hints *jsonHints
	root  *shape
	// ctx says where in the document the next value is: the keys' positions
	// that lead to it, whichever array it is an element of.
	ctx uint32
	// strs is strings made and not yet used, and strRoom how many bytes of
	// them may still be made together (newString).
	strs    []String
	strRoom int
	// esc is the buffer a string with an escape is built in, and text the
	// batch it goes on to (keepText).
	esc  []byte
	text []byte

	// records asks for a jsonRecord of each value, which a reviver needs;
	// last is the record of the value parsed most recently, and recScratch
	// is scratch for the records of arrays' elements.
	records    bool
	last       *jsonRecord
	recScratch []*jsonRecord

	// doc is the document, in a runtime with a memory limit, and nil
	// otherwise: its strings then go by the rule of kept, and none shares
	// an allocation with another (jsonKept).
	doc *String
}

// jsonHintBits sizes jsonHints, and jsonKeyBits jsonState.keys. A
// document has few places objects are made in, each of few layouts; the
// names a program's documents use are more, and each is one word.
const (
	jsonHintBits = 7
	jsonKeyBits  = 8
	jsonMetBits  = 12
)

// jsonState is what JSON keeps from one call to the next, made at its first
// use: a runtime that uses no JSON pays one word for it.
type jsonState struct {
	// keys is the atoms of the keys hashed lately, by their text. An entry
	// is only a guess, which is compared with the name of the atom it holds.
	keys [1 << jsonKeyBits]Atom
	// values is a parser's scratch between calls, empty.
	values []Value
	// met is the layouts met once, as bits (metBefore), and metCount how
	// many are set.
	met      [1 << (jsonMetBits - 6)]uint64
	metCount int
	// enc is an encoder between calls: a call made from a toJSON, a
	// replacer or a getter, while another is out, makes its own.
	enc *jsonEncoder
}

// jsonKeepValues and jsonKeepBytes are the most of a parser's scratch, in
// values, and of an encoder's buffer, in bytes, kept between calls. A document
// past them is large enough to pay for its buffer, and is not left holding it.
const (
	jsonKeepValues = 1 << 14
	jsonKeepBytes  = 1 << 16
)

// jsonHints is, for each place in a document (jsonParser.ctx), the shape of
// the last object parsed there, which was reached from the realm's root by
// transitions of plain data properties, and stays valid: a shape in the tree
// never changes, and a place that two kinds of object share is a guess that
// fails at a key (parseObject). It is a realm's, so that its prototype is the
// one a parse there makes objects of: a shape holds its prototype, and a
// runtime's table would hold the realm of the last object parsed.
type jsonHints [1 << jsonHintBits]*shape

// jsonCaches is the runtime's jsonState.
func (r *Runtime) jsonCaches() *jsonState {
	if r.json == nil {
		r.json = new(jsonState)
	}
	return r.json
}

// parseJSON parses the whole of src.
func (r *Runtime) parseJSON(src string, records bool, doc *String) (Value, *jsonRecord, error) {
	st := r.jsonCaches()
	p := jsonParser{rt: r, src: src, st: st, records: records, scratch: st.values, strRoom: len(src), doc: doc}
	// No script runs while the text is parsed, so the levels the parser may
	// enter are known when it starts: jsonLevelsPerNest to each level of the
	// Go recursion left, past which nest would refuse the next.
	p.room = (maxGoRecursion - r.nesting) * jsonLevelsPerNest
	st.values = nil
	if !records {
		if root := rootFor(ClassObject, r.proto.object); root != nil && !root.unique && !root.loose && root.tree != nil {
			p.root = root
			if r.Realm.jsonHints == nil {
				r.Realm.jsonHints = new(jsonHints)
			}
			p.hints = r.Realm.jsonHints
		}
	}
	v, err := p.parseText()
	clear(p.scratch)
	if cap(p.scratch) <= jsonKeepValues {
		st.values = p.scratch[:0]
	}
	return v, p.last, err
}

// parseText parses one value and the space around it.
func (p *jsonParser) parseText() (Value, error) {
	p.skipSpace()
	v, err := p.parseValue()
	if err != nil {
		return Undefined, err
	}
	p.skipSpace()
	if p.pos != len(p.src) {
		return Undefined, p.rt.throwSyntaxError("unexpected trailing content in JSON at position %d", p.pos)
	}
	return v, nil
}

// jsonRecord is what JSON.parse remembers of a value for a reviver: the value
// itself, and for a primitive the text it was parsed from, or for an array or
// object the records of what it holds. A key given twice keeps the record of
// the value it ended up with, the last.
type jsonRecord struct {
	value    Value
	source   string
	elements []*jsonRecord
	entries  map[Atom]*jsonRecord
}

// skipSpace steps over white space, of which minified text has none.
func (p *jsonParser) skipSpace() {
	if p.pos < len(p.src) && p.src[p.pos] > ' ' {
		return
	}
	p.skipSpaces()
}

func (p *jsonParser) skipSpaces() {
	for p.pos < len(p.src) {
		switch p.src[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *jsonParser) parseValue() (Value, error) {
	start := p.pos
	v, err := p.parseValueAt()
	if err == nil && p.records && !v.IsObject() {
		p.last = &jsonRecord{value: v, source: p.src[start:p.pos]}
	}
	return v, err
}

// parseValueAt is parseValue without the record, which an array or an object
// makes for itself.
func (p *jsonParser) parseValueAt() (Value, error) {
	if p.pos >= len(p.src) {
		return Undefined, p.rt.throwSyntaxError("unexpected end of JSON input")
	}
	c := p.src[p.pos]
	switch c {
	case '"':
		return p.parseStringValue()
	case '{', '[':
		if p.room <= 0 {
			// nest's error, the error of runaway recursion.
			return Undefined, p.rt.throwRangeError("maximum call stack size exceeded")
		}
		p.room--
		var v Value
		var err error
		if c == '{' {
			v, err = p.parseObject()
		} else {
			v, err = p.parseArray()
		}
		p.room++
		return v, err
	case 't':
		if strings.HasPrefix(p.src[p.pos:], "true") {
			p.pos += 4
			return True, nil
		}
	case 'f':
		if strings.HasPrefix(p.src[p.pos:], "false") {
			p.pos += 5
			return False, nil
		}
	case 'n':
		if strings.HasPrefix(p.src[p.pos:], "null") {
			p.pos += 4
			return Null, nil
		}
	default:
		if c == '-' || (c >= '0' && c <= '9') {
			return p.parseNumber()
		}
	}
	return Undefined, p.rt.throwSyntaxError(
		"unexpected token %q in JSON at position %d", p.src[p.pos], p.pos)
}
