package vm

import (
	intl "github.com/go-quickjs/go-intl"
)

// Intl.Segmenter: where a text may be broken into characters, words or
// sentences. go-intl's Segmenter breaks it, with ICU's rules and
// dictionaries; what is here is the object a script sees, which hands out
// the pieces one at a time along with where each one started.

type segmenterOptions struct {
	segmenter   *intl.Segmenter
	locale      string
	granularity string // grapheme, word, sentence
}

// segmentsData is one text, already broken, its pieces counted in code
// units, as a script indexes it.
type segmentsData struct {
	options *segmenterOptions
	// input is the text as it was handed over, which the pieces are cut from so
	// that an unpaired surrogate survives the round trip.
	input    *String
	segments *intl.Segments
	all      []intl.Segment
}

// segmentIterData is how far through the pieces an iterator has got.
type segmentIterData struct {
	segments *segmentsData
	i        int
}

func (r *Runtime) initSegmenter(intlObj *Object) {
	proto := newObject(r.proto.object, ClassObject)
	ctor := r.newCtor("Segmenter", 0, proto, func(rt *Runtime, this Value, args []Value) (Value, error) {
		proto, err := rt.protoFromNewTargetErr(rt.intlProtoOf("Segmenter"))
		if err != nil {
			return Undefined, err
		}
		if !rt.Constructing() {
			return Undefined, rt.intlRequiresNew("Intl.Segmenter")
		}
		defer rt.enterIntl("Intl.Segmenter")()
		tags, err := rt.requestedLocales(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		options, err := rt.strictOptions(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		matcher, err := rt.stringOption(options, "localeMatcher", "best fit", "lookup", "best fit")
		if err != nil {
			return Undefined, err
		}
		loc, err := rt.intlLocale(intl.ServiceSegmenter, tags, matcher)
		if err != nil {
			return Undefined, err
		}
		o := &segmenterOptions{}
		if o.granularity, err = rt.stringOption(options, "granularity", "grapheme",
			"grapheme", "word", "sentence"); err != nil {
			return Undefined, err
		}
		opts := intl.SegmenterOptions{Granularity: map[string]intl.Granularity{
			"grapheme": intl.GranularityGrapheme, "word": intl.GranularityWord,
			"sentence": intl.GranularitySentence}[o.granularity]}
		if o.segmenter, err = intl.NewSegmenter(loc, opts); err != nil {
			return Undefined, rt.intlInternal()
		}
		// The Segmenter uses none of the Unicode extension; ICU's variant
		// POSIX, which V8 keeps as -u-va-posix in every service, stays.
		kept := loc
		kept.Attributes, kept.Keywords = nil, nil
		if va, ok := loc.Keyword("va"); ok {
			kept.Keywords = []intl.Keyword{{Key: "va", Value: va}}
		}
		o.locale = kept.String()
		out := newObject(proto, ClassObject)
		out.data = o
		return Obj(out), nil
	})
	r.defValue(intlObj, "Segmenter", Obj(ctor))
	r.intlProtos["Segmenter"] = proto
	r.defToStringTag(proto, "Intl.Segmenter")
	r.defSupportedLocalesOfService(ctor, intl.ServiceSegmenter)

	segments := r.newSegmentsProto()
	iter := r.newSegmentIteratorProto()
	r.intlProtos["Segments"] = segments
	r.intlProtos["SegmentIterator"] = iter

	r.defMethod(proto, "segment", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		o, err := rt.segmenterOf(this, "Intl.Segmenter.prototype.segment")
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
		o, err := rt.segmenterOf(this, "Intl.Segmenter.prototype.resolvedOptions")
		if err != nil {
			return Undefined, err
		}
		out := newObject(rt.proto.object, ClassObject)
		rt.putString(out, "locale", o.locale)
		rt.putString(out, "granularity", o.granularity)
		return Obj(out), nil
	})
}

// breakUp finds every place the text may be broken, in its code units,
// which go-intl reads as JavaScript holds them.
func (o *segmenterOptions) breakUp(s *String) *segmentsData {
	units := make([]uint16, s.Len())
	for i := range units {
		units[i] = uint16(s.CharCodeAt(i))
	}
	segments := o.segmenter.Segment(units)
	return &segmentsData{options: o, input: s, segments: segments, all: segments.All()}
}

// dataObject is what a script is handed for one piece: the piece itself, where
// it started, the text it came from, and -- for words -- whether it is a word
// rather than a space or a mark.
func (r *Runtime) dataObject(d *segmentsData, seg intl.Segment) Value {
	out := newObject(r.proto.object, ClassObject)
	piece := d.input.Substring(seg.Index, seg.End)
	out.setOwnRaw(r.atoms.intern("segment"), Str(piece), propDefault)
	out.setOwnRaw(r.atoms.intern("index"), Int(seg.Index), propDefault)
	out.setOwnRaw(r.atoms.intern("input"), Str(d.input), propDefault)
	if d.options.granularity == "word" {
		out.setOwnRaw(r.atoms.intern("isWordLike"), Bool(seg.IsWordLike), propDefault)
	}
	return Obj(out)
}

func (r *Runtime) newSegmentsProto() *Object {
	p := newObject(r.proto.object, ClassObject)
	r.defMethod(p, "containing", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		d, err := rt.segmentsOf(this, "%Segments.prototype%.containing")
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
		seg, ok := d.segments.Containing(int(n))
		if !ok {
			return Undefined, nil
		}
		return rt.dataObject(d, seg), nil
	})
	r.defSymbolMethod(p, r.wellKnown.iterator, "[Symbol.iterator]", 0,
		func(rt *Runtime, this Value, args []Value) (Value, error) {
			d, err := rt.segmentsOf(this, "%SegmentIsPrototype%[@@iterator]")
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
			return Undefined, rt.intlIncompatibleReceiver("%SegmentIterator.prototype%.next", this)
		}
		if it.i >= len(it.segments.all) {
			return Obj(rt.iterResult(Undefined, true)), nil
		}
		v := rt.dataObject(it.segments, it.segments.all[it.i])
		it.i++
		return Obj(rt.iterResult(v, false)), nil
	})
	r.defToStringTag(p, "Segmenter String Iterator")
	return p
}

func (r *Runtime) segmenterOf(this Value, method string) (*segmenterOptions, error) {
	if o := this.Object(); o != nil {
		if opts, ok := o.data.(*segmenterOptions); ok {
			return opts, nil
		}
	}
	return nil, r.intlIncompatibleReceiver(method, this)
}

func (r *Runtime) segmentsOf(this Value, method string) (*segmentsData, error) {
	if o := this.Object(); o != nil {
		if d, ok := o.data.(*segmentsData); ok {
			return d, nil
		}
	}
	return nil, r.intlIncompatibleReceiver(method, this)
}
