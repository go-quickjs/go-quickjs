package vm

import (
	"sort"

	"github.com/go-quickjs/go-quickjs/internal/icu"
	"github.com/go-quickjs/go-quickjs/internal/wtf8"
)

// Intl.Segmenter: where a text may be broken into characters, words or
// sentences. The rules live in internal/icu; what is here is the object a
// script sees, which hands out the pieces one at a time along with where each
// one started.

type segmenterOptions struct {
	locale      *icu.Locale
	requested   string
	granularity string // grapheme, word, sentence
}

// segmentsData is one text, already broken, with every piece addressable both
// by the byte it starts at and by the code unit a script would call its index.
type segmentsData struct {
	options *segmenterOptions
	// input is the text as it was handed over, which the pieces are cut from so
	// that an unpaired surrogate survives the round trip.
	input *String
	// text is the same, with anything unpaired made well formed, which is what
	// the rules read. Both spellings are three bytes, so the offsets agree.
	text string
	// at is where each piece begins and where the last one ends, in bytes, and
	// units is the same in code units.
	at    []int
	units []int
}

// segmentIterData is how far through the pieces an iterator has got.
type segmentIterData struct {
	segments *segmentsData
	i        int
}

func (r *Runtime) initSegmenter(intl *Object) {
	proto := newObject(r.proto.object, ClassObject)
	ctor := r.newCtor("Segmenter", 0, proto, func(rt *Runtime, this Value, args []Value) (Value, error) {
		proto, err := rt.protoFromNewTargetErr(rt.intlProtoOf("Segmenter"))
		if err != nil {
			return Undefined, err
		}
		if err := rt.requireNew("Intl.Segmenter"); err != nil {
			return Undefined, err
		}
		tags, err := rt.requestedLocales(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		options, err := rt.strictOptions(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		if _, err := rt.stringOption(options, "localeMatcher", "best fit",
			"lookup", "best fit"); err != nil {
			return Undefined, err
		}
		choice := rt.resolveLocale(tags)
		o := &segmenterOptions{locale: choice.data, requested: choice.locale()}
		if o.granularity, err = rt.stringOption(options, "granularity", "grapheme",
			"grapheme", "word", "sentence"); err != nil {
			return Undefined, err
		}
		out := newObject(proto, ClassObject)
		out.data = o
		return Obj(out), nil
	})
	r.defValue(intl, "Segmenter", Obj(ctor))
	r.intlProtos["Segmenter"] = proto
	r.defToStringTag(proto, "Intl.Segmenter")
	r.defSupportedLocalesOf(ctor)

	segments := r.newSegmentsProto()
	iter := r.newSegmentIteratorProto()
	r.intlProtos["Segments"] = segments
	r.intlProtos["SegmentIterator"] = iter

	r.defMethod(proto, "segment", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.segmenterOf(this)
		if err != nil {
			return Undefined, err
		}
		s, err := rt.toString(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		out := newObject(segments, ClassObject)
		out.data = o.breakUp(s)
		return Obj(out), nil
	})
	r.defMethod(proto, "resolvedOptions", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.segmenterOf(this)
		if err != nil {
			return Undefined, err
		}
		out := newObject(rt.proto.object, ClassObject)
		rt.putString(out, "locale", o.requested)
		rt.putString(out, "granularity", o.granularity)
		return Obj(out), nil
	})
}

// breakUp finds every place the text may be broken, and where each piece falls
// in the text as a script counts it.
func (o *segmenterOptions) breakUp(s *String) *segmentsData {
	text := s.Go()
	if !wtf8.WellFormed(text) {
		text = wtf8.ToWellFormed(text)
	}
	var kind icu.Granularity
	switch o.granularity {
	case "word":
		kind = icu.Words
	case "sentence":
		kind = icu.Sentences
	}
	at := icu.Breaks(text, kind)
	d := &segmentsData{options: o, input: s, text: text, at: at}

	// The same places counted in code units, which is what a script indexes by.
	d.units = make([]int, len(at))
	unit, k := 0, 0
	for i := 0; i <= len(text); {
		for k < len(at) && at[k] == i {
			d.units[k] = unit
			k++
		}
		if i == len(text) {
			break
		}
		switch {
		case text[i] < 0x80:
			i, unit = i+1, unit+1
		default:
			r, size := wtf8.DecodeRune(text[i:])
			if r > 0xFFFF {
				unit += 2
			} else {
				unit++
			}
			i += size
		}
	}
	return d
}

// pieces is how many there are.
func (d *segmentsData) pieces() int {
	if len(d.at) == 0 {
		return 0
	}
	return len(d.at) - 1
}

// dataObject is what a script is handed for one piece: the piece itself, where
// it started, the text it came from, and -- for words -- whether it is a word
// rather than a space or a mark.
func (r *Runtime) dataObject(d *segmentsData, i int) Value {
	out := newObject(r.proto.object, ClassObject)
	piece := d.input.Substring(d.units[i], d.units[i+1])
	out.setOwnRaw(r.atoms.intern("segment"), Str(piece), propDefault)
	out.setOwnRaw(r.atoms.intern("index"), Int(d.units[i]), propDefault)
	out.setOwnRaw(r.atoms.intern("input"), Str(d.input), propDefault)
	if d.options.granularity == "word" {
		out.setOwnRaw(r.atoms.intern("isWordLike"),
			Bool(icu.WordLike(d.text[d.at[i]:d.at[i+1]])), propDefault)
	}
	return Obj(out)
}

func (r *Runtime) newSegmentsProto() *Object {
	p := newObject(r.proto.object, ClassObject)
	r.defMethod(p, "containing", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		d, err := rt.segmentsOf(this)
		if err != nil {
			return Undefined, err
		}
		n, err := rt.toInteger(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		if n < 0 || n >= float64(d.input.Len()) {
			return Undefined, nil
		}
		// The piece this code unit fell in: the last one that began at or
		// before it.
		i := sort.SearchInts(d.units, int(n)+1) - 1
		if i < 0 || i >= d.pieces() {
			return Undefined, nil
		}
		return rt.dataObject(d, i), nil
	})
	r.defSymbolMethod(p, r.wellKnown.iterator, "[Symbol.iterator]", 0,
		func(rt *Runtime, this Value, args []Value) (Value, error) {
			d, err := rt.segmentsOf(this)
			if err != nil {
				return Undefined, err
			}
			out := newObject(rt.intlProtoOf("SegmentIterator"), ClassObject)
			out.data = &segmentIterData{segments: d}
			return Obj(out), nil
		})
	return p
}

func (r *Runtime) newSegmentIteratorProto() *Object {
	p := newObject(r.proto.iterator, ClassObject)
	r.defMethod(p, "next", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		var it *segmentIterData
		if this.IsObject() {
			it, _ = this.Object().data.(*segmentIterData)
		}
		if it == nil {
			return Undefined, rt.throwTypeError(
				"Segment Iterator.prototype.next called on an incompatible receiver")
		}
		if it.i >= it.segments.pieces() {
			return Obj(rt.iterResult(Undefined, true)), nil
		}
		v := rt.dataObject(it.segments, it.i)
		it.i++
		return Obj(rt.iterResult(v, false)), nil
	})
	r.defToStringTag(p, "Segmenter String Iterator")
	return p
}

func (r *Runtime) segmenterOf(this Value) (*segmenterOptions, error) {
	if o := this.Object(); o != nil {
		if opts, ok := o.data.(*segmenterOptions); ok {
			return opts, nil
		}
	}
	return nil, r.throwTypeError("this is not an Intl.Segmenter")
}

func (r *Runtime) segmentsOf(this Value) (*segmentsData, error) {
	if o := this.Object(); o != nil {
		if d, ok := o.data.(*segmentsData); ok {
			return d, nil
		}
	}
	return nil, r.throwTypeError("this is not a Segments object")
}
