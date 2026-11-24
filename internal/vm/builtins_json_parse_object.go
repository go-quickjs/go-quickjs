package vm

import (
	"math/bits"
	"strconv"
	"strings"
	"unicode/utf8"
	"unsafe"

	"github.com/go-quickjs/go-quickjs/internal/jsnum"
	"github.com/go-quickjs/go-quickjs/internal/jsonscan"
	"github.com/go-quickjs/go-quickjs/internal/wtf8"
)

// JSON.parse's parser after parseValueAt: its objects, arrays, strings and
// numbers. The file is the layout lock's position JO (docs/optimizing.md),
// so that what changes in the parser before it moves none of this.

// parseObject parses an object by walking the shape tree (see jsonParser).
func (p *jsonParser) parseObject() (Value, error) {
	root := p.root
	if root == nil {
		return p.parseObjectEntries()
	}
	p.pos++ // consume '{'
	p.skipSpace()
	if p.pos < len(p.src) && p.src[p.pos] == '}' {
		p.pos++
		return Obj(newShapedLiteral(root, ClassObject, 0)), nil
	}
	ctx := p.ctx
	slot := &p.hints[ctx>>(32-jsonHintBits)]
	hint := *slot
	cur := root
	mark := len(p.scratch)
	n := int32(0)
	for {
		p.skipSpace()
		if p.pos >= len(p.src) || p.src[p.pos] != '"' {
			return Undefined, p.rt.throwSyntaxError(
				"expected a property name in JSON at position %d", p.pos)
		}
		key, err := p.parseString()
		if err != nil {
			return Undefined, err
		}
		// While the object is the hint's, the hint says the shape, and cur
		// stands still. A hint is only a guess, and nothing it says is
		// taken on trust: each key is compared with the name in its place,
		// so that the keys so far are the hint's first n, and its ancestor
		// of n properties the shape they make, the one a walk by
		// transitions would have come to.
		if hint != nil && (n >= hint.n || !p.rt.atoms.names(hint.keys[n].key, key)) {
			cur = hint.ancestorAt(n)
			hint = nil
		}
		if hint == nil {
			c := p.nextShape(cur, key)
			if c == nil {
				// The key is still to come, and any after it: such an object
				// is given room, or the first of them would move it.
				return p.parseEntries(p.objectOf(cur, mark, jsonRoomLeft), nil, key)
			}
			cur = c
		}
		p.skipSpace()
		if p.pos >= len(p.src) || p.src[p.pos] != ':' {
			return Undefined, p.rt.throwSyntaxError("expected ':' in JSON at position %d", p.pos)
		}
		p.pos++
		p.skipSpace()
		n++
		p.ctx = (ctx + uint32(n)) * 0x9E3779B1
		v, err := p.parseValueAt()
		if err != nil {
			return Undefined, err
		}
		p.scratch = append(p.scratch, v)
		p.skipSpace()
		if p.pos >= len(p.src) {
			return Undefined, p.rt.throwSyntaxError("unexpected end of JSON input")
		}
		switch p.src[p.pos] {
		case ',':
			p.pos++
		case '}':
			p.pos++
			s := cur
			if hint != nil {
				s = hint.ancestorAt(n)
			}
			if s != *slot {
				*slot = remember(s)
			}
			return Obj(p.objectPair(s, mark)), nil
		default:
			return Undefined, p.rt.throwSyntaxError(
				"expected ',' or '}' in JSON at position %d", p.pos)
		}
	}
}

// nextShape is the shape of an object of shape s given key as its next
// property, or nil if the walk cannot go there: s has the key, or the tree
// will not hold the shape.
func (p *jsonParser) nextShape(s *shape, key string) *shape {
	if s.next != nil && s.nextFlags == propDefault && p.rt.atoms.names(s.nextKey, key) {
		return s.next
	}
	a := p.internKey(key)
	e := shapeEdge{a, propDefault}
	if s.next != nil {
		if s.nextEdge() == e {
			return s.next
		}
		if c := s.more[e]; c != nil {
			return c
		}
	}
	// No transition: s may have the key. A table long enough to want an
	// index is asked by the object, which has one.
	if s.n > linearScanLimit {
		return nil
	}
	for _, k := range s.keys[:s.n] {
		if k.key == a {
			return nil
		}
	}
	return s.child(e)
}

// internKey is the atom of a key, which may be a slice of the document.
func (p *jsonParser) internKey(key string) Atom {
	atoms := p.rt.atoms
	e := &p.st.keys[jsonKeyHash(key)]
	if atoms.names(*e, key) {
		return *e
	}
	a := atoms.internCopy(key)
	*e = a
	return a
}

// jsonKeyHash is a slot of jsonState.keys for a key, from its length and its
// ends, which is where the keys of a document differ.
func jsonKeyHash(key string) uint32 {
	n := len(key)
	var h uint64
	switch {
	case n >= 8:
		h = wtf8.Load64(key, 0) ^ wtf8.Load64(key, n-8)<<1
	case n > 0:
		h = uint64(key[0]) | uint64(key[n/2])<<8 | uint64(key[n-1])<<16
	}
	return uint32((h + uint64(n)) * 0x9E3779B97F4A7C15 >> (64 - jsonKeyBits))
}

// jsonRoomLeft is the room an object is made with for keys the walk could not
// take.
const jsonRoomLeft = 2

// objectOf makes the object of shape s whose values are those on scratch from
// mark, and takes them off it, with room for more.
func (p *jsonParser) objectOf(s *shape, mark, more int) *Object {
	vals := p.scratch[mark:]
	n := len(vals)
	o := newShapedLiteral(s, ClassObject, n+more)
	copy(o.vals()[:n], vals)
	o.valn = uint32(n)
	if n > linearScanLimit && s.index == nil {
		o.buildIndex()
	}
	clear(vals)
	p.scratch = p.scratch[:mark]
	if verifyShapes {
		checkShape(o)
	}
	return o
}

// parseObjectEntries is parseObject for an object built by the table: all of
// them while a reviver wants records of what they hold.
func (p *jsonParser) parseObjectEntries() (Value, error) {
	// How many properties there are is not known without parsing them, and
	// growing the table from nothing costs an allocation per doubling. Room
	// for a few, in the object's own allocation, pays for itself by the second
	// key.
	o := newLiteralObject(p.rt.proto.object, ClassObject, 0)
	var rec *jsonRecord
	if p.records {
		rec = &jsonRecord{value: Obj(o), entries: make(map[Atom]*jsonRecord)}
	}
	p.pos++ // consume '{'
	p.skipSpace()
	if p.pos < len(p.src) && p.src[p.pos] == '}' {
		p.pos++
		p.last = rec
		return Obj(o), nil
	}
	key, err := p.parseKey()
	if err != nil {
		return Undefined, err
	}
	return p.parseEntries(o, rec, key)
}

// parseKey parses a property name.
func (p *jsonParser) parseKey() (string, error) {
	p.skipSpace()
	if p.pos >= len(p.src) || p.src[p.pos] != '"' {
		return "", p.rt.throwSyntaxError(
			"expected a property name in JSON at position %d", p.pos)
	}
	return p.parseString()
}

// parseEntries parses what is left of an object, from the colon after key.
func (p *jsonParser) parseEntries(o *Object, rec *jsonRecord, key string) (Value, error) {
	for {
		p.skipSpace()
		if p.pos >= len(p.src) || p.src[p.pos] != ':' {
			return Undefined, p.rt.throwSyntaxError("expected ':' in JSON at position %d", p.pos)
		}
		p.pos++
		p.skipSpace()
		v, err := p.parseValue()
		if err != nil {
			return Undefined, err
		}
		atom := p.internKey(key)
		o.setOwnRaw(atom, v, propDefault)
		if rec != nil {
			rec.entries[atom] = p.last
		}
		p.skipSpace()
		if p.pos >= len(p.src) {
			return Undefined, p.rt.throwSyntaxError("unexpected end of JSON input")
		}
		switch p.src[p.pos] {
		case ',':
			p.pos++
		case '}':
			p.pos++
			p.last = rec
			return Obj(o), nil
		default:
			return Undefined, p.rt.throwSyntaxError(
				"expected ',' or '}' in JSON at position %d", p.pos)
		}
		if key, err = p.parseKey(); err != nil {
			return Undefined, err
		}
	}
}

func (p *jsonParser) parseArray() (Value, error) {
	p.pos++ // consume '['
	p.skipSpace()
	if p.pos < len(p.src) && p.src[p.pos] == ']' {
		p.pos++
		a := Obj(p.rt.newArrayFrom(nil))
		if p.records {
			p.last = &jsonRecord{value: a}
		}
		return a, nil
	}
	ctx := p.ctx
	mark := len(p.scratch)
	recMark := len(p.recScratch)
	for {
		p.skipSpace()
		p.ctx = ctx
		var v Value
		var err error
		if p.records {
			v, err = p.parseValue()
			p.recScratch = append(p.recScratch, p.last)
		} else {
			v, err = p.parseValueAt()
		}
		if err != nil {
			return Undefined, err
		}
		p.scratch = append(p.scratch, v)
		p.skipSpace()
		if p.pos >= len(p.src) {
			return Undefined, p.rt.throwSyntaxError("unexpected end of JSON input")
		}
		switch p.src[p.pos] {
		case ',':
			p.pos++
		case ']':
			p.pos++
			elems := p.scratch[mark:]
			a := Obj(p.rt.newArrayFrom(elems))
			clear(elems)
			p.scratch = p.scratch[:mark]
			if p.records {
				recs := make([]*jsonRecord, len(p.recScratch)-recMark)
				copy(recs, p.recScratch[recMark:])
				clear(p.recScratch[recMark:])
				p.recScratch = p.recScratch[:recMark]
				p.last = &jsonRecord{value: a, elements: recs}
			}
			return a, nil
		default:
			return Undefined, p.rt.throwSyntaxError(
				"expected ',' or ']' in JSON at position %d", p.pos)
		}
	}
}

// The word-at-a-time tests of a string's text: each has the top bit of a byte
// of its result set if that byte, or a lower one, is what it looks for, so
// that the lowest bit set is the first such byte, exactly. A subtraction
// borrows only from a byte that is itself found.
const (
	jsonOnes   = 0x0101010101010101
	jsonHigh   = 0x8080808080808080
	jsonQuotes = 0x2222222222222222 // '"'
	jsonSlashs = 0x5C5C5C5C5C5C5C5C // '\\'
	jsonSpaces = 0x2020202020202020 // the first character that is not a control
	jsonLeadED = 0xEDEDEDEDEDEDEDED // what a surrogate's three bytes begin with
)

// jsonStops finds in w a quotation mark, a backslash, a control character or
// a byte that is not ASCII.
func jsonStops(w uint64) uint64 {
	return (w | (w ^ jsonQuotes - jsonOnes) | (w ^ jsonSlashs - jsonOnes) | (w - jsonSpaces)) & jsonHigh
}

// jsonStopsWide is jsonStops for text that may have bytes above ASCII, which
// are stepped over. A byte's own top bit would otherwise be taken for a find.
func jsonStopsWide(w uint64) uint64 {
	q, b := w^jsonQuotes, w^jsonSlashs
	return ((q-jsonOnes)&^q | (b-jsonOnes)&^b | (w-jsonSpaces)&^w) & jsonHigh
}

// jsonWordsFirst is how many words of a string are read before it is taken for
// a long one.
const jsonWordsFirst = 4

// jsonPlainEnd is where the plain text from i ends: the index of the first
// quotation mark, backslash or control character from there, or the text's
// length, and whether the text is all ASCII to there.
func jsonPlainEnd(s string, i int) (int, bool) {
	n := len(s)
	ascii := true
	// s is the rest of the document, whose strings are short: the scan of
	// long ones is for one that has got this far.
	for words := 0; i+8 <= n; words++ {
		if words == jsonWordsFirst {
			if i += jsonscan.ASCII(s[i:]); i+8 > n {
				break
			}
		}
		if m := jsonStops(wtf8.Load64(s, i)); m != 0 {
			i += bits.TrailingZeros64(m) >> 3
			if s[i] < utf8.RuneSelf {
				return i, true
			}
			ascii = false
			break
		}
		i += 8
	}
	if ascii {
		for ; i < n; i++ {
			c := s[i]
			if c >= utf8.RuneSelf {
				ascii = false
				break
			}
			if c == '"' || c == '\\' || c < 0x20 {
				return i, true
			}
		}
		if ascii {
			return n, true
		}
	}
	for words := 0; i+8 <= n; words++ {
		// Bytes above ASCII are stepped over: a quotation mark is stopped
		// at anyway.
		if words == jsonWordsFirst {
			if i += jsonscan.Wide(s[i:], '"'); i+8 > n {
				break
			}
		}
		if m := jsonStopsWide(wtf8.Load64(s, i)); m != 0 {
			return i + bits.TrailingZeros64(m)>>3, false
		}
		i += 8
	}
	for ; i < n; i++ {
		if c := s[i]; c == '"' || c == '\\' || c < 0x20 {
			return i, false
		}
	}
	return n, false
}

// parseString parses a string: a property name, which only the shape tree or
// the atom table goes on to keep, or the text of a value.
func (p *jsonParser) parseString() (string, error) {
	s, _, err := p.parseStringASCII()
	return s, err
}

// parseStringASCII is parseString, which says too if a string it could slice
// out of the document is all ASCII.
func (p *jsonParser) parseStringASCII() (string, bool, error) {
	p.pos++ // consume '"'

	// A string with no escape in it -- which nearly every string in real JSON
	// is -- is a slice of the source text, costing neither a copy nor an
	// allocation. Sharing the bytes is what the engine's own substrings do.
	start := p.pos
	i, ascii := jsonPlainEnd(p.src, start)
	if i >= len(p.src) {
		p.pos = i
		return "", false, p.rt.throwSyntaxError("unterminated string in JSON")
	}
	if c := p.src[i]; c == '"' {
		p.pos = i + 1
		return p.src[start:i], ascii, nil
	} else if c != '\\' {
		p.pos = i
		return "", false, p.rt.throwSyntaxError(
			"a control character is not allowed in a JSON string at position %d", i)
	}
	return p.parseEscaped(start, i, ascii)
}

// parseEscaped builds the string from start, whose first escape is at i, and
// says if it is all ASCII, as the text before i is if ascii says so.
func (p *jsonParser) parseEscaped(start, i int, ascii bool) (string, bool, error) {
	buf := append(p.esc[:0], p.src[start:i]...)
	p.pos = i
	for {
		if p.pos >= len(p.src) {
			return "", false, p.rt.throwSyntaxError("unterminated string in JSON")
		}
		c := p.src[p.pos]
		switch {
		case c == '"':
			p.pos++
			p.esc = buf
			return p.keepText(buf), ascii, nil
		case c == '\\':
			p.pos++
			if p.pos >= len(p.src) {
				return "", false, p.rt.throwSyntaxError("unterminated escape in JSON")
			}
			var b byte
			switch p.src[p.pos] {
			case '"':
				b = '"'
			case '\\':
				b = '\\'
			case '/':
				b = '/'
			case 'n':
				b = '\n'
			case 'r':
				b = '\r'
			case 't':
				b = '\t'
			case 'b':
				b = '\b'
			case 'f':
				b = '\f'
			case 'u':
				r, err := p.parseUnicodeEscape()
				if err != nil {
					return "", false, err
				}
				if r >= utf8.RuneSelf {
					ascii = false
				}
				if r >= 0xDC00 && r <= 0xDFFF {
					// Text can end in a high surrogate just as an escape
					// can, and the two are then one character (AppendJoin).
					buf = wtf8.AppendJoin(buf, string(wtf8.AppendRune(nil, r)))
					continue
				}
				// A lone surrogate is preserved rather than replaced.
				buf = wtf8.AppendRune(buf, r)
				continue
			default:
				return "", false, p.rt.throwSyntaxError(
					"invalid escape %q in JSON at position %d", p.src[p.pos], p.pos)
			}
			buf = append(buf, b)
			p.pos++
		case c < 0x20:
			return "", false, p.rt.throwSyntaxError(
				"a control character is not allowed in a JSON string at position %d", p.pos)
		default:
			// Escapes come close together where there are any, and the text
			// between them is taken a byte at a time for as long as it is
			// short.
			j, end := p.pos, min(p.pos+jsonShortRun, len(p.src))
			for j < end {
				if c := p.src[j]; c < 0x20 || c == '"' || c == '\\' || c >= utf8.RuneSelf {
					break
				}
				j++
			}
			if j == end || j < len(p.src) && p.src[j] >= utf8.RuneSelf {
				var plain bool
				if j, plain = jsonPlainEnd(p.src, j); !plain {
					ascii = false
				}
			}
			if c == 0xED {
				// May be a low surrogate, after an escape of a high one.
				buf = wtf8.AppendJoin(buf, p.src[p.pos:j])
			} else {
				buf = append(buf, p.src[p.pos:j]...)
			}
			p.pos = j
		}
	}
}

// jsonShortRun is how many bytes of text between escapes are read one by one.
const jsonShortRun = 8

// jsonTextBatch is the most text made room for at once.
const jsonTextBatch = 1 << 12

// keepText is a string of the text an escaped string came to. A document's
// are written one after another into a batch, one allocation for many, which
// a string that outlives the rest holds: no more than jsonTextBatch, and no
// more than is left of the document it would otherwise be a slice of.
func (p *jsonParser) keepText(b []byte) string {
	if len(b) > jsonTextBatch/8 || p.doc != nil {
		return string(b)
	}
	if cap(p.text)-len(p.text) < len(b) {
		p.text = make([]byte, 0, max(min(len(p.src)-p.pos, jsonTextBatch), len(b)))
	}
	at := len(p.text)
	p.text = append(p.text, b...)
	return unsafe.String(unsafe.SliceData(p.text[at:]), len(b))
}

// parseStringValue parses a string that is a value.
func (p *jsonParser) parseStringValue() (Value, error) {
	s, ascii, err := p.parseStringASCII()
	if err != nil {
		return Undefined, err
	}
	if s == "" {
		return Str(emptyString), nil
	}
	if p.doc != nil {
		return jsonKept[struct{}](p, s), nil
	}
	if !ascii {
		// WTF-8 joined from an escape's pairs is what NewString reads, as it
		// reads any other.
		ascii, length := scanString(s)
		out := p.newString()
		out.s, out.length, out.ascii = s, length, ascii
		if !ascii {
			out.endsHigh, out.startsLow = wtf8.UnpairedEnds(s)
		}
		return Str(out), nil
	}
	out := p.newString()
	out.s, out.length, out.ascii = s, len(s), true
	return Str(out), nil
}

// jsonMaxStrs is the most strings made at once.
const jsonMaxStrs = 32

// newString is a new empty String. A document's strings are made a batch at a
// time, which is one allocation for many, and a string that outlives the rest
// holds its batch, as it holds the document its text is a slice of. So that
// what the batches of a document hold together is never more than its size
// again, whichever strings outlive it -- one of each record's ten had held
// every String of a document, four times its size -- they take strRoom's
// bytes in all, and the strings past that one allocation each. A batch is
// smaller than what is left of the document too, which has room for that
// many strings.
func (p *jsonParser) newString() *String {
	if len(p.strs) == 0 {
		size := int(unsafe.Sizeof(String{}))
		n := min((len(p.src)-p.pos)/(2*size), p.strRoom/size, jsonMaxStrs)
		if n < 2 {
			return new(String)
		}
		p.strRoom -= n * size
		p.strs = make([]String, n)
	}
	s := &p.strs[0]
	p.strs = p.strs[1:]
	return s
}

// parseUnicodeEscape reads \uXXXX, combining a surrogate pair when the next
// escape completes one.
func (p *jsonParser) parseUnicodeEscape() (rune, error) {
	if p.pos+4 >= len(p.src) {
		return 0, p.rt.throwSyntaxError("truncated \\u escape in JSON")
	}
	v, err := strconv.ParseUint(p.src[p.pos+1:p.pos+5], 16, 32)
	if err != nil {
		return 0, p.rt.throwSyntaxError("invalid \\u escape in JSON at position %d", p.pos)
	}
	p.pos += 5
	r := rune(v)
	if r >= 0xD800 && r <= 0xDBFF && strings.HasPrefix(p.src[p.pos:], `\u`) && p.pos+6 <= len(p.src) {
		if lo, err := strconv.ParseUint(p.src[p.pos+2:p.pos+6], 16, 32); err == nil {
			if lo >= 0xDC00 && lo <= 0xDFFF {
				p.pos += 6
				return 0x10000 + (r-0xD800)<<10 + (rune(lo) - 0xDC00), nil
			}
		}
	}
	return r, nil
}

// parseNumber reads a JSON number, which is a strict subset of the JavaScript
// numeric grammar: no leading plus, no leading zeros, no hexadecimal, and a
// digit required on both sides of a decimal point.
func (p *jsonParser) parseNumber() (Value, error) {
	// An integer, the commonest number there is, ends at a comma or a bracket.
	i := p.pos
	var m uint64
	for i < len(p.src) {
		d := p.src[i] - '0'
		if d > 9 {
			break
		}
		m = m*10 + uint64(d)
		i++
	}
	if n := i - p.pos; n > 0 && n < 16 && i < len(p.src) && (p.src[i] == ',' || p.src[i] == ']' || p.src[i] == '}') &&
		(n == 1 || p.src[p.pos] != '0') {
		p.pos = i
		return Float(float64(m)), nil
	}
	if f, end, ok := jsnum.ParseShort(p.src, p.pos); ok {
		p.pos = end
		return Float(f), nil
	}
	start := p.pos
	if p.pos < len(p.src) && p.src[p.pos] == '-' {
		p.pos++
	}
	digitsStart := p.pos
	for p.pos < len(p.src) && p.src[p.pos] >= '0' && p.src[p.pos] <= '9' {
		p.pos++
	}
	if p.pos == digitsStart {
		return Undefined, p.rt.throwSyntaxError("invalid number in JSON at position %d", start)
	}
	// A leading zero may not be followed by another digit.
	if p.src[digitsStart] == '0' && p.pos-digitsStart > 1 {
		return Undefined, p.rt.throwSyntaxError("invalid number in JSON at position %d", start)
	}
	if p.pos < len(p.src) && p.src[p.pos] == '.' {
		p.pos++
		fracStart := p.pos
		for p.pos < len(p.src) && p.src[p.pos] >= '0' && p.src[p.pos] <= '9' {
			p.pos++
		}
		if p.pos == fracStart {
			return Undefined, p.rt.throwSyntaxError("invalid number in JSON at position %d", start)
		}
	}
	if p.pos < len(p.src) && (p.src[p.pos] == 'e' || p.src[p.pos] == 'E') {
		p.pos++
		if p.pos < len(p.src) && (p.src[p.pos] == '+' || p.src[p.pos] == '-') {
			p.pos++
		}
		expStart := p.pos
		for p.pos < len(p.src) && p.src[p.pos] >= '0' && p.src[p.pos] <= '9' {
			p.pos++
		}
		if p.pos == expStart {
			return Undefined, p.rt.throwSyntaxError("invalid number in JSON at position %d", start)
		}
	}
	f, err := jsnum.ParseFloat(p.src[start:p.pos])
	if err != nil {
		// Overflow yields infinity, which is the same result JavaScript gives.
		if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
			return Float(f), nil
		}
		return Undefined, p.rt.throwSyntaxError("invalid number in JSON at position %d", start)
	}
	return Float(f), nil
}
