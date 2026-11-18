package decoder

import "github.com/go-quickjs/go-quickjs/wasm/internal/module"

// Stream decodes and validates a module as its bytes arrive: each section
// once all of it is there, and in the code section each function body as it
// completes, validated then. What Finish returns is what Decode and then
// ValidateFunctions return for the whole module: the same module, or the
// same first error at the same offset. The stream checks in the order they
// do, with their code, but it stops at the first error it finds, and at an
// error -- or wherever the bytes it has do not settle a check the whole
// module would -- Finish decodes and validates the whole module again, so
// that which error is reported is always theirs: a body's error, say, may
// lose to a malformed section after the code section, and a function's
// error names it from the name section, which comes after the bodies.
//
// A Stream is not safe for concurrent use.
type Stream struct {
	lim *Limits
	md  *moduleDecoder
	v   *funcValidator
	// buf holds the bytes so far, md.b and md.m.Bytes the same.
	buf []byte
	// pos is where the next section starts.
	pos int
	// header is set once the header is decoded.
	header bool
	// In the code section: where it ends, the bodies still to come, and
	// the index of the next one among the module's functions.
	inCode   bool
	codeEnd  int
	codeLeft uint32
	codeNext uint32
	// stopped is set at an error, or at a module past the size limit:
	// Finish then decodes the whole module.
	stopped bool
}

// NewStream returns a stream decoding a module under limits l. sizeHint, if
// positive, is the module's expected size, which the buffer is made for.
func NewStream(l *Limits, sizeHint int) *Stream {
	s := &Stream{lim: l}
	if sizeHint > 0 && uint64(sizeHint) <= moduleSizeLimit(l) {
		s.buf = make([]byte, 0, sizeHint)
	}
	md := &moduleDecoder{m: &module.Module{}, lim: l}
	md.stopOnError = true
	md.nextOrdered = secType
	s.md = md
	s.v = &funcValidator{m: md.m, lim: l}
	return s
}

// Write adds p, the module's next bytes, and decodes and validates what they
// complete.
func (s *Stream) Write(p []byte) {
	if len(p) == 0 {
		return
	}
	if s.stopped {
		s.buf = append(s.buf, p...)
		return
	}
	s.buf = append(s.buf, p...)
	s.md.b, s.md.m.Bytes = s.buf, s.buf
	if uint64(len(s.buf)) > moduleSizeLimit(s.lim) {
		s.stopped = true
		return
	}
	defer func() {
		// A bug of the stream's must not take the host down: the whole
		// module is decoded again.
		if recover() != nil {
			s.stopped = true
		}
	}()
	s.advance()
}

// Finish returns the module of all the bytes written, decoded and with its
// function bodies validated, or the first error Decode or ValidateFunctions
// finds in it.
func (s *Stream) Finish() (*module.Module, *Error) {
	md := s.md
	if !s.stopped && s.header && !s.inCode && s.pos == len(s.buf) {
		md.pc, md.end = s.pos, len(s.buf)
		md.checkMismatchedCounts()
		if md.ok() {
			md.m.Customs = md.customs
			return md.m, nil
		}
	}
	m, err := Decode(s.buf, s.lim)
	if err != nil {
		return nil, err
	}
	if err := ValidateFunctions(m, s.lim); err != nil {
		return nil, err
	}
	return m, nil
}

// ready reports whether the bytes from pos hold a whole LEB128 of at most
// five bytes before end -- or five bytes, whose reading settles it either
// way -- and where it ends.
func (s *Stream) ready(pos, end int) (int, bool) {
	end = min(end, len(s.buf))
	for i := 0; i < 5; i++ {
		if pos+i >= end {
			return 0, false
		}
		if s.buf[pos+i]&0x80 == 0 {
			return pos + i + 1, true
		}
	}
	return pos + 5, true
}

// advance decodes what the bytes so far complete.
func (s *Stream) advance() {
	md := s.md
	if !s.header {
		if len(s.buf) < 8 {
			return
		}
		md.pc, md.end = 0, 8
		md.decodeHeader()
		if md.failed() {
			s.stopped = true
			return
		}
		s.header, s.pos = true, 8
	}
	for !s.stopped {
		if s.inCode {
			if !s.codeBodies() {
				return
			}
			continue
		}
		if s.pos >= len(s.buf) {
			return
		}
		lenEnd, ok := s.ready(s.pos+1, len(s.buf))
		if !ok {
			return
		}
		itd := newDecoder(s.buf, s.pos, len(s.buf))
		length, _ := itd.readU32v(s.pos+1, "section length")
		if itd.failed() {
			s.stopped = true
			return
		}
		secEnd := lenEnd + int(length)
		if s.buf[s.pos] == secCode && secEnd > len(s.buf) {
			// The code section is decoded body by body.
			if !s.startCode(lenEnd, secEnd) {
				return
			}
			continue
		}
		if secEnd > len(s.buf) {
			return
		}
		// The section is all here: it is decoded as Decode decodes it.
		it := &sectionIterator{d: itd, sectionStart: s.pos, secEnd: s.pos}
		it.next()
		if itd.failed() {
			s.stopped = true
			return
		}
		if it.custom {
			md.customs = append(md.customs, module.CustomSection{
				Name: string(s.buf[it.customName.Offset:it.customName.End()]),
				Data: module.Range{Offset: uint32(it.payloadStart), Len: uint32(it.secEnd - it.payloadStart)},
			})
		}
		if it.code != secUnknown {
			md.decodeSection(it.code, it.payloadStart, it.secEnd)
			if md.failed() {
				s.stopped = true
				return
			}
			if it.code == secCode {
				// Its bodies, all here, are validated now.
				for i := md.m.NumImportedFunctions; i < uint32(len(md.m.Functions)); i++ {
					if !s.validate(i) {
						return
					}
				}
			}
		}
		s.pos = secEnd
	}
}

// startCode starts the code section, whose payload starts at start and ends
// at end, past the bytes so far, once its count is here: what
// decodeSection and decodeCodeSection do before the bodies. It reports
// whether it started it.
func (s *Stream) startCode(start, end int) bool {
	md := s.md
	if _, ok := s.ready(start, end); !ok {
		return false
	}
	md.pc, md.end = start, end
	if !md.checkSectionOrder(secCode) {
		s.stopped = true
		return false
	}
	sectionStart := md.pc
	count := md.consumeU32v("functions count")
	if count != md.numDeclaredFunctions {
		md.errorf(sectionStart, "function body count %d mismatch (%d expected)", count, md.numDeclaredFunctions)
	}
	if md.failed() {
		s.stopped = true
		return false
	}
	s.inCode, s.codeEnd, s.codeLeft, s.codeNext = true, end, count, 0
	return true
}

// codeBodies decodes and validates the bodies the bytes so far complete, as
// decodeCodeSection reads them, and ends the section after the last. It
// reports whether the section has ended.
func (s *Stream) codeBodies() bool {
	md := s.md
	m := md.m
	for s.codeLeft > 0 {
		pos := md.pc
		sizeEnd, ok := s.ready(pos, s.codeEnd)
		if !ok {
			return false
		}
		size, _ := md.readU32v(pos, "body size")
		// A body is taken once all of it is here, or all of the section
		// is, whose end it then runs past.
		if md.ok() && uint64(sizeEnd)+uint64(size) > uint64(len(s.buf)) && len(s.buf) < s.codeEnd {
			return false
		}
		size = md.consumeU32v("body size")
		if md.ok() && size > md.lim.FunctionSize {
			md.errorf(pos, "size %d > maximum function size %d", size, md.lim.FunctionSize)
		}
		if md.failed() {
			s.stopped = true
			return false
		}
		offset := md.pc
		md.consumeBytes(size)
		if md.failed() {
			s.stopped = true
			return false
		}
		i := m.NumImportedFunctions + s.codeNext
		m.Functions[i].Code = module.Range{Offset: uint32(offset), Len: size}
		s.codeNext++
		s.codeLeft--
		if !s.validate(i) {
			return false
		}
	}
	md.codeSeen = true
	if md.pc != s.codeEnd {
		// The section is not the size it says, which decodeSection reports
		// after it, and Finish does.
		s.stopped = true
		return false
	}
	s.inCode = false
	s.pos = s.codeEnd
	return true
}

// validate validates function i's body, stopping the stream at an error.
func (s *Stream) validate(i uint32) bool {
	s.v.m = s.md.m
	s.v.validateFunction(&s.md.m.Functions[i])
	if s.v.err != nil {
		s.stopped = true
		return false
	}
	return true
}
