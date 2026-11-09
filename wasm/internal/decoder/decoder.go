// Package decoder decodes and validates WebAssembly modules.
//
// It follows V8's decoders (src/wasm/decoder.h, module-decoder-impl.h,
// function-body-decoder-impl.h) closely enough that a module V8 rejects is
// rejected here with V8's message, at V8's offset, for the first error V8
// finds: which error a module reports first is part of what an embedder sees.
// Portions are derived from V8, Copyright the V8 project authors, used under
// the BSD license in V8_LICENSE.
package decoder

import (
	"fmt"
	"strconv"
)

// Error is a decoding or validation error: V8's message and the offset in the
// module's bytes it was found at.
type Error struct {
	Offset uint32
	Msg    string
}

// Error returns the message as V8 reports it in a CompileError, without the
// API's prefix: "<message> @+<offset>".
func (e *Error) Error() string {
	return e.Msg + " @+" + strconv.FormatUint(uint64(e.Offset), 10)
}

// Decoder reads a module's bytes. Positions are offsets into the whole
// module, so that an error's offset is the module offset V8 reports. Only the
// first error is kept; later reads past it are harmless and report nothing.
type Decoder struct {
	b   []byte
	pc  int
	end int
	err *Error
	// stopOnError moves pc to end at the first error, as V8's module decoder
	// does, so that a loop over a section's entries ends.
	stopOnError bool
}

// newDecoder returns a decoder over b[pc:end].
func newDecoder(b []byte, pc, end int) *Decoder {
	return &Decoder{b: b, pc: pc, end: end}
}

func (d *Decoder) ok() bool     { return d.err == nil }
func (d *Decoder) failed() bool { return d.err != nil }
func (d *Decoder) more() bool   { return d.pc < d.end }

// errorAt records msg as the error at position pos, unless there is one.
func (d *Decoder) errorAt(pos int, msg string) {
	if d.err != nil {
		return
	}
	d.err = &Error{Offset: uint32(pos), Msg: msg}
	if d.stopOnError {
		d.pc = d.end
	}
}

// errorf records a formatted error at position pos.
func (d *Decoder) errorf(pos int, format string, args ...any) {
	if d.err != nil {
		return
	}
	d.errorAt(pos, fmt.Sprintf(format, args...))
}

// readU8 returns the byte at pos, or an error named msg if there is none.
func (d *Decoder) readU8(pos int, msg string) byte {
	if pos >= d.end {
		d.errorAt(pos, msg)
		return 0
	}
	return d.b[pos]
}

// readU32 returns the little-endian uint32 at pos.
func (d *Decoder) readU32(pos int, msg string) uint32 {
	if d.end-pos < 4 {
		d.errorAt(pos, msg)
		return 0
	}
	return uint32(d.b[pos]) | uint32(d.b[pos+1])<<8 | uint32(d.b[pos+2])<<16 | uint32(d.b[pos+3])<<24
}

// readU64 returns the little-endian uint64 at pos.
func (d *Decoder) readU64(pos int, msg string) uint64 {
	if d.end-pos < 8 {
		d.errorAt(pos, msg)
		return 0
	}
	return uint64(d.readU32(pos, msg)) | uint64(d.readU32(pos+4, msg))<<32
}

// readLEB reads a LEB128 integer of bits bits at pos, returning its value and
// length; a signed one is sign-extended. On an error it returns 0, 0.
func (d *Decoder) readLEB(pos int, bits int, signed bool, name string) (uint64, int) {
	if pos < d.end && d.b[pos]&0x80 == 0 {
		v := uint64(d.b[pos])
		if signed && v&0x40 != 0 {
			v |= ^uint64(0x7f)
		}
		return v, 1
	}
	maxLen := (bits + 6) / 7
	var result uint64
	for i := 0; i < maxLen; i++ {
		p := pos + i
		last := i == maxLen-1
		atEnd := p >= d.end
		var b byte
		if !atEnd {
			b = d.b[p]
			result |= uint64(b&0x7f) << (7 * i)
		}
		if !last && b&0x80 != 0 {
			continue
		}
		if atEnd || b&0x80 != 0 {
			what := "length overflow"
			if atEnd {
				what = "reached end"
			}
			d.errorf(p, "%s while decoding %s", what, name)
			return 0, 0
		}
		if last {
			extra := bits - (maxLen-1)*7
			signExt := extra
			if signed {
				signExt--
			}
			checked := b & byte(0xff<<signExt)
			signExtended := byte(0x7f) & byte(0xff<<signExt)
			if checked != 0 && !(signed && checked == signExtended) {
				d.errorAt(p, "extra bits in varint")
				return 0, 0
			}
		}
		shift := 7 * (i + 1)
		if signed && shift < 64 && b&0x40 != 0 {
			result |= ^uint64(0) << shift
		}
		if !signed && bits < 64 {
			result &= 1<<bits - 1
		}
		return result, i + 1
	}
	return 0, 0
}

func (d *Decoder) readU32v(pos int, name string) (uint32, int) {
	v, n := d.readLEB(pos, 32, false, name)
	return uint32(v), n
}

func (d *Decoder) readI32v(pos int, name string) (int32, int) {
	v, n := d.readLEB(pos, 32, true, name)
	return int32(v), n
}

func (d *Decoder) readU64v(pos int, name string) (uint64, int) {
	return d.readLEB(pos, 64, false, name)
}

func (d *Decoder) readI64v(pos int, name string) (int64, int) {
	v, n := d.readLEB(pos, 64, true, name)
	return int64(v), n
}

func (d *Decoder) readI33v(pos int, name string) (int64, int) {
	v, n := d.readLEB(pos, 33, true, name)
	return int64(v), n
}

// checkAvailable reports whether n bytes remain, recording V8's error if not.
func (d *Decoder) checkAvailable(n uint32) bool {
	if uint64(n) > uint64(d.end-d.pc) {
		d.errorf(d.pc, "expected %d bytes, fell off end", n)
		return false
	}
	return true
}

func (d *Decoder) consumeU8(name string) byte {
	if !d.checkAvailable(1) {
		d.pc = d.end
		return 0
	}
	b := d.b[d.pc]
	d.pc++
	return b
}

func (d *Decoder) consumeU32(name string) uint32 {
	if !d.checkAvailable(4) {
		d.pc = d.end
		return 0
	}
	v := d.readU32(d.pc, name)
	d.pc += 4
	return v
}

func (d *Decoder) consumeU32v(name string) uint32 {
	v, n := d.readU32v(d.pc, name)
	d.pc += n
	return v
}

func (d *Decoder) consumeI32v(name string) int32 {
	v, n := d.readI32v(d.pc, name)
	d.pc += n
	return v
}

func (d *Decoder) consumeU64v(name string) uint64 {
	v, n := d.readU64v(d.pc, name)
	d.pc += n
	return v
}

func (d *Decoder) consumeI64v(name string) int64 {
	v, n := d.readI64v(d.pc, name)
	d.pc += n
	return v
}

// consumeBytes skips n bytes, or to the end with an error if fewer remain.
func (d *Decoder) consumeBytes(n uint32) {
	if d.checkAvailable(n) {
		d.pc += int(n)
	} else {
		d.pc = d.end
	}
}

// lookahead reports whether the byte offset bytes past pc is expected.
func (d *Decoder) lookahead(offset int, expected byte) bool {
	return d.end-d.pc > offset && d.b[d.pc+offset] == expected
}
