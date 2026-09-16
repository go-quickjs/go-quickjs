package vm

import (
	"strconv"
	"strings"

	"github.com/go-quickjs/go-quickjs/internal/wtf8"
)

// Annex B.
//
// These are the legacy built-ins the specification keeps only because the web
// depends on them. None is a good idea in new code -- escape predates
// encodeURIComponent and mangles anything outside Latin-1, and the HTML methods
// build markup by concatenation, which is how injection bugs happen -- but a
// host running existing code needs them to exist.

func (r *Runtime) initAnnexB() {
	r.defMethod(r.global, "escape", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, err := rt.toString(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(escapeLegacy(s.Go()))), nil
	})

	r.defMethod(r.global, "unescape", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		s, err := rt.toString(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		return Str(NewString(unescapeLegacy(s.Go()))), nil
	})

	r.initHTMLStringMethods()
	r.initLegacyDateMethods()
}

// escapeLegacy implements escape, which predates encodeURIComponent.
//
// It leaves the unreserved ASCII set alone, writes %XX for the rest of Latin-1
// and %uXXXX for anything above it -- a form nothing else understands, which is
// most of why encodeURIComponent exists.
func escapeLegacy(s string) string {
	const keep = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789@*_+-./"
	var b strings.Builder
	for _, u := range wtf8.ToUTF16(s) {
		switch {
		case u < 128 && strings.IndexByte(keep, byte(u)) >= 0:
			b.WriteByte(byte(u))
		case u < 256:
			b.WriteString("%" + pad(strings.ToUpper(strconv.FormatUint(uint64(u), 16)), 2))
		default:
			b.WriteString("%u" + pad(strings.ToUpper(strconv.FormatUint(uint64(u), 16)), 4))
		}
	}
	return b.String()
}

// unescapeLegacy reverses escape, leaving anything malformed as written.
func unescapeLegacy(s string) string {
	units := wtf8.ToUTF16(s)
	out := make([]uint16, 0, len(units))
	for i := 0; i < len(units); i++ {
		if units[i] != '%' {
			out = append(out, units[i])
			continue
		}
		if i+5 < len(units) && units[i+1] == 'u' {
			if v, ok := hex4(units[i+2 : i+6]); ok {
				out = append(out, v)
				i += 5
				continue
			}
		}
		if i+2 < len(units) {
			if v, ok := hex4(units[i+1 : i+3]); ok {
				out = append(out, v)
				i += 2
				continue
			}
		}
		out = append(out, units[i])
	}
	return wtf8.FromUTF16(out)
}

// hex4 reads a run of hex digits as a code unit.
func hex4(units []uint16) (uint16, bool) {
	var v uint16
	for _, u := range units {
		d, ok := hexDigit(byte(u))
		if !ok || u > 127 {
			return 0, false
		}
		v = v<<4 | uint16(d)
	}
	return v, true
}

// initHTMLStringMethods defines the markup-building String methods.
//
// Each wraps the receiver in a tag, and the attribute form escapes only the
// double quote -- which is the whole of the specified behaviour, and the reason
// none of these should be used to build markup from untrusted input.
func (r *Runtime) initHTMLStringMethods() {
	p := r.proto.str

	wrap := func(name, tag, attr string, length int) {
		r.defMethod(p, name, length, func(rt *Runtime, this Value, args []Value) (Value, error) {
			if this.IsNullish() {
				return Undefined, rt.throwTypeError(
					"String.prototype.%s called on %s", name, rt.describe(this))
			}
			s, err := rt.toString(this)
			if err != nil {
				return Undefined, err
			}
			open := "<" + tag
			if attr != "" {
				v, err := rt.toString(arg(args, 0))
				if err != nil {
					return Undefined, err
				}
				open += " " + attr + "=\"" + strings.ReplaceAll(v.Go(), "\"", "&quot;") + "\""
			}
			return Str(NewString(open + ">" + s.Go() + "</" + tag + ">")), nil
		})
	}

	wrap("anchor", "a", "name", 1)
	wrap("link", "a", "href", 1)
	wrap("fontcolor", "font", "color", 1)
	wrap("fontsize", "font", "size", 1)
	for _, m := range []struct{ name, tag string }{
		{"big", "big"}, {"blink", "blink"}, {"bold", "b"}, {"fixed", "tt"},
		{"italics", "i"}, {"small", "small"}, {"strike", "strike"},
		{"sub", "sub"}, {"sup", "sup"},
	} {
		wrap(m.name, m.tag, "", 0)
	}
}

// initLegacyDateMethods defines getYear, setYear and toGMTString.
//
// getYear reports the year minus 1900, which is the bug that made the year 2000
// interesting; it survives because pages still call it.
func (r *Runtime) initLegacyDateMethods() {
	p := r.proto.date
	if p == nil {
		return
	}

	r.defMethod(p, "getYear", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		full, err := rt.callDateMethod(this, "getFullYear", nil)
		if err != nil {
			return Undefined, err
		}
		if !full.IsNumber() || full.Number() != full.Number() {
			return full, nil
		}
		return Float(full.Number() - 1900), nil
	})

	r.defMethod(p, "setYear", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		n, err := rt.toNumber(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		// A two-digit year means 19xx, which is the whole point of the method.
		if n >= 0 && n <= 99 {
			n += 1900
		}
		return rt.callDateMethod(this, "setFullYear", []Value{Float(n)})
	})

	r.defMethod(p, "toGMTString", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		return rt.callDateMethod(this, "toUTCString", nil)
	})
}

// callDateMethod forwards to another Date method, which is how the legacy ones
// are defined in terms of the modern ones.
func (r *Runtime) callDateMethod(this Value, name string, args []Value) (Value, error) {
	fn, err := r.getValueProp(this, r.atoms.intern(name))
	if err != nil {
		return Undefined, err
	}
	if !isCallable(fn) {
		return Undefined, r.throwTypeError("Date.prototype.%s is not available", name)
	}
	return r.call(fn, this, args)
}
