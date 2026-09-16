package vm

import (
	"encoding/base64"
	"strings"
)

// Uint8Array's base64 and hex conversions.
//
// Every runtime grew its own: atob and btoa in the browser, Buffer in Node,
// a hand-rolled loop everywhere else. These put the conversion where the bytes
// already are, and give the caller control over the two things hand-rolled
// versions habitually get wrong -- which alphabet, and what to do with a
// trailing chunk that is not a whole group.

// base64Options is the resolved options bag the conversions take.
type base64Options struct {
	// url selects the base64url alphabet, which replaces + and / with - and _
	// so that the result is safe in a URL.
	url bool
	// lastChunk says how to treat input whose length is not a multiple of four.
	lastChunk lastChunkMode
	// omitPadding drops the trailing = when encoding.
	omitPadding bool
}

type lastChunkMode uint8

const (
	// chunkLoose accepts a final chunk with the padding missing, which is what
	// most producers emit.
	chunkLoose lastChunkMode = iota
	// chunkStrict requires the padding, and rejects non-zero bits that the
	// discarded part of a partial chunk would carry.
	chunkStrict
	// chunkStopBeforePartial stops at a partial chunk and reports how far it
	// read, which is what a streaming decoder needs.
	chunkStopBeforePartial
)

// readBase64Options resolves the options argument.
func (r *Runtime) readBase64Options(v Value, forEncode bool) (base64Options, error) {
	opts := base64Options{}
	if v.IsUndefined() {
		return opts, nil
	}
	if !v.IsObject() {
		return opts, r.throwTypeError("the options argument must be an object")
	}

	alphabet, err := r.getValueProp(v, r.atoms.intern("alphabet"))
	if err != nil {
		return opts, err
	}
	if !alphabet.IsUndefined() {
		s, err := r.toString(alphabet)
		if err != nil {
			return opts, err
		}
		switch s.Go() {
		case "base64":
		case "base64url":
			opts.url = true
		default:
			return opts, r.throwTypeError("the alphabet must be \"base64\" or \"base64url\"")
		}
	}

	if forEncode {
		omit, err := r.getValueProp(v, r.atoms.intern("omitPadding"))
		if err != nil {
			return opts, err
		}
		opts.omitPadding = omit.Truthy()
		return opts, nil
	}

	handling, err := r.getValueProp(v, r.atoms.intern("lastChunkHandling"))
	if err != nil {
		return opts, err
	}
	if !handling.IsUndefined() {
		s, err := r.toString(handling)
		if err != nil {
			return opts, err
		}
		switch s.Go() {
		case "loose":
		case "strict":
			opts.lastChunk = chunkStrict
		case "stop-before-partial":
			opts.lastChunk = chunkStopBeforePartial
		default:
			return opts, r.throwTypeError(
				"lastChunkHandling must be \"loose\", \"strict\" or \"stop-before-partial\"")
		}
	}
	return opts, nil
}

func (o base64Options) encoding() *base64.Encoding {
	enc := base64.StdEncoding
	if o.url {
		enc = base64.URLEncoding
	}
	if o.omitPadding {
		return enc.WithPadding(base64.NoPadding)
	}
	return enc
}

func (r *Runtime) initBase64Builtins(uint8Ctor, uint8Proto *Object) {
	r.defMethod(uint8Ctor, "fromBase64", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, err := rt.base64Input(arg(args, 0), "fromBase64")
		if err != nil {
			return Undefined, err
		}
		opts, err := rt.readBase64Options(arg(args, 1), false)
		if err != nil {
			return Undefined, err
		}
		out, _, err := rt.decodeBase64(s, opts)
		if err != nil {
			return Undefined, err
		}
		return Obj(rt.newUint8ArrayFrom(out)), nil
	})

	r.defMethod(uint8Ctor, "fromHex", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, err := rt.base64Input(arg(args, 0), "fromHex")
		if err != nil {
			return Undefined, err
		}
		out, _, err := rt.decodeHex(s, len(s)/2)
		if err != nil {
			return Undefined, err
		}
		return Obj(rt.newUint8ArrayFrom(out)), nil
	})

	r.defMethod(uint8Proto, "toBase64", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		t, err := rt.uint8ArrayOf(this, "Uint8Array.prototype.toBase64")
		if err != nil {
			return Undefined, err
		}
		opts, err := rt.readBase64Options(arg(args, 0), true)
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(opts.encoding().EncodeToString(t))), nil
	})

	r.defMethod(uint8Proto, "toHex", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		t, err := rt.uint8ArrayOf(this, "Uint8Array.prototype.toHex")
		if err != nil {
			return Undefined, err
		}
		const digits = "0123456789abcdef"
		var b strings.Builder
		b.Grow(len(t) * 2)
		for _, c := range t {
			b.WriteByte(digits[c>>4])
			b.WriteByte(digits[c&0xF])
		}
		return Str(NewString(b.String())), nil
	})

	// The setFrom variants decode into an existing array, reporting how much of
	// the input they consumed. That is what makes streaming possible: the
	// caller keeps the unread tail and prepends it to the next piece.
	r.defMethod(uint8Proto, "setFromBase64", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		t, err := rt.uint8ArrayOf(this, "Uint8Array.prototype.setFromBase64")
		if err != nil {
			return Undefined, err
		}
		s, err := rt.base64Input(arg(args, 0), "setFromBase64")
		if err != nil {
			return Undefined, err
		}
		opts, err := rt.readBase64Options(arg(args, 1), false)
		if err != nil {
			return Undefined, err
		}
		out, read, err := rt.decodeBase64Into(s, opts, len(t))
		if err != nil {
			return Undefined, err
		}
		copy(t, out)
		return Obj(rt.readWritten(read, len(out))), nil
	})

	r.defMethod(uint8Proto, "setFromHex", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		t, err := rt.uint8ArrayOf(this, "Uint8Array.prototype.setFromHex")
		if err != nil {
			return Undefined, err
		}
		s, err := rt.base64Input(arg(args, 0), "setFromHex")
		if err != nil {
			return Undefined, err
		}
		out, read, err := rt.decodeHex(s, len(t))
		if err != nil {
			return Undefined, err
		}
		copy(t, out)
		return Obj(rt.readWritten(read, len(out))), nil
	})
}

// base64Input validates the string argument, which is deliberately not coerced.
func (r *Runtime) base64Input(v Value, name string) (string, error) {
	if !v.IsString() {
		return "", r.throwTypeError("%s requires a string", name)
	}
	return v.String().Go(), nil
}

// readWritten builds the { read, written } result the setFrom methods return.
func (r *Runtime) readWritten(read, written int) *Object {
	o := newObject(r.proto.object, ClassObject)
	o.setOwnRaw(r.atoms.intern("read"), Int(read), propDefault)
	o.setOwnRaw(r.atoms.intern("written"), Int(written), propDefault)
	return o
}

// uint8ArrayOf recovers the bytes a Uint8Array views.
//
// Only Uint8Array qualifies: the conversions are about bytes, and a view of
// wider elements has a byte order the caller would have to reason about.
func (r *Runtime) uint8ArrayOf(this Value, name string) ([]byte, error) {
	t, err := r.typedArrayOf(this, name)
	if err != nil {
		return nil, err
	}
	if t.kind != elemUint8 {
		return nil, r.throwTypeError("%s requires a Uint8Array", name)
	}
	b := t.storage().bytes
	return b[t.byteOffset : t.byteOffset+t.length], nil
}

// newUint8ArrayFrom wraps bytes in a fresh Uint8Array.
func (r *Runtime) newUint8ArrayFrom(b []byte) *Object {
	buf := newObject(r.arrayBufferProto, ClassArrayBuffer)
	buf.data = &arrayBufferData{bytes: b}
	o := newObject(r.uint8Proto, ClassTypedArray)
	o.data = &typedArrayData{buffer: buf, kind: elemUint8, length: len(b)}
	return o
}

// decodeBase64 decodes a whole string.
func (r *Runtime) decodeBase64(s string, opts base64Options) ([]byte, int, error) {
	return r.decodeBase64Into(s, opts, -1)
}

// decodeBase64Into decodes at most max bytes, reporting how many characters of
// the input were consumed.
//
// Decoding is done a four-character group at a time rather than by handing the
// whole string to encoding/base64, because the caller needs the read count and
// because the three lastChunkHandling modes disagree about exactly the last
// group.
func (r *Runtime) decodeBase64Into(s string, opts base64Options, max int) ([]byte, int, error) {
	alphabet := base64Alphabet
	if opts.url {
		alphabet = base64URLAlphabet
	}

	var out []byte
	// chunk accumulates the 6-bit values of the group being read.
	var chunk [4]byte
	n := 0
	// read is the index just past the last character that contributed to a
	// completed group, which is what the caller resumes from.
	read := 0
	sawPadding := false

	for i := 0; i < len(s); i++ {
		c := s[i]
		if isBase64Whitespace(c) {
			continue
		}
		if c == '=' {
			sawPadding = true
			// Padding may only appear where a group is already 2 or 3 deep;
			// anywhere else it is not padding but a stray character.
			if n < 2 {
				return nil, 0, r.throwSyntaxError("unexpected padding in base64 input")
			}
			// Everything after the first padding character must be padding or
			// whitespace.
			for j := i + 1; j < len(s); j++ {
				if isBase64Whitespace(s[j]) {
					continue
				}
				if s[j] != '=' {
					return nil, 0, r.throwSyntaxError("unexpected character after base64 padding")
				}
			}
			break
		}

		v := strings.IndexByte(alphabet, c)
		if v < 0 {
			return nil, 0, r.throwSyntaxError("invalid character in base64 input")
		}
		chunk[n] = byte(v)
		n++
		if n < 4 {
			continue
		}
		if max >= 0 && len(out)+3 > max {
			// The destination is full, and a partial group is never written.
			return out, read, nil
		}
		out = append(out,
			chunk[0]<<2|chunk[1]>>4,
			chunk[1]<<4|chunk[2]>>2,
			chunk[2]<<6|chunk[3])
		n = 0
		read = i + 1
	}

	if n == 0 {
		return out, len(s), nil
	}
	// A group of one is never valid: a single 6-bit value cannot carry a byte.
	if n == 1 {
		return nil, 0, r.throwSyntaxError("truncated base64 input")
	}
	if opts.lastChunk == chunkStopBeforePartial && !sawPadding {
		// Leave the partial group unread, so the caller can prepend it to
		// whatever arrives next.
		return out, read, nil
	}
	if opts.lastChunk == chunkStrict && !sawPadding {
		return nil, 0, r.throwSyntaxError("base64 input is missing its padding")
	}

	tail := []byte{chunk[0]<<2 | chunk[1]>>4}
	if opts.lastChunk == chunkStrict && chunk[n-1]<<(8-2*(4-uint(n))) != 0 {
		// The bits beyond the last whole byte must be zero, or the encoding
		// carries information the decoder is about to discard.
		return nil, 0, r.throwSyntaxError("base64 input has non-zero padding bits")
	}
	if n == 3 {
		tail = append(tail, chunk[1]<<4|chunk[2]>>2)
	}
	if max >= 0 && len(out)+len(tail) > max {
		return out, read, nil
	}
	return append(out, tail...), len(s), nil
}

// decodeHex decodes a hex string, writing at most max bytes.
func (r *Runtime) decodeHex(s string, max int) ([]byte, int, error) {
	if len(s)%2 != 0 {
		return nil, 0, r.throwSyntaxError("a hex string must have an even length")
	}
	var out []byte
	for i := 0; i+1 < len(s); i += 2 {
		if max >= 0 && len(out) >= max {
			return out, i, nil
		}
		hi, ok1 := hexDigit(s[i])
		lo, ok2 := hexDigit(s[i+1])
		if !ok1 || !ok2 {
			return nil, 0, r.throwSyntaxError("invalid character in hex input")
		}
		out = append(out, hi<<4|lo)
	}
	return out, len(s), nil
}

func hexDigit(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// isBase64Whitespace reports the characters that may appear between groups.
//
// Encoders wrap long output at a fixed column, so ignoring whitespace is what
// makes a decoder able to read what an encoder produced.
func isBase64Whitespace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f'
}

const (
	base64Alphabet    = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	base64URLAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
)
