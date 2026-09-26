package vm

import (
	"math"
	"strconv"

	intl "github.com/go-quickjs/go-intl"
)

// go-intl, which Intl is moving onto a service at a time, from
// internal/icu. Each service builds its go-intl formatter from the options
// it has read, in the order and with the errors ECMA-402 gives, and answers
// with what the formatter writes.

// intlCompat is where the runtime answers as Node does rather than as the
// standard: everywhere under WithNodeQuirks, and otherwise everywhere but
// the places its standards mode corrects -- the twelve-hour clock hour12
// chooses, the Islamic eras before the Hijrah, the formats of Temporal
// values, a Unicode extension keyword's value "yes", the currencies
// DisplayNames names, a duration's fraction of a second summed exactly
// rather than in an int64, the separator a digital duration's minutes take
// when its hours are left out, the fields a date pattern's literals seem to
// write, the deprecated Islamic calendars, when a resolved locale keeps its
// hour cycle, the zone a plain Temporal value is read in, the time zone
// names a DateTimeFormat takes and reports, a coptic year before the era,
// the days of the Chinese and Korean calendars, which are Temporal's, the
// region of a locale's hour cycles, and the time zones in no region.
func (r *Runtime) intlCompat() intl.Compat {
	if r.nodeQuirks {
		return intl.NodeICU
	}
	return intl.NodeICU &^ (intl.TwelveHourCycle | intl.IslamicEras | intl.TemporalFormats | intl.YesValues |
		intl.CurrencyNames | intl.DurationOverflow | intl.DurationSeparator | intl.LiteralFields |
		intl.IslamicFallback | intl.HourCycleKeyword | intl.PlainValueZone | intl.ZoneIdentifiers |
		intl.CopticEra | intl.ChineseAstronomy | intl.SubdivisionHourCycles | intl.RegionZones)
}

// canonicalizer puts locale identifiers in canonical form, as
// Intl.getCanonicalLocales does.
func (r *Runtime) canonicalizer() (*intl.Canonicalizer, error) {
	return intl.NewCanonicalizer(intl.Embedded, intl.CanonicalizeOptions{Compat: r.intlCompat()})
}

// intlDefaultLocale is ECMA-402's DefaultLocale: the runtime's language, in
// canonical form, or American English where that cannot be read.
func (r *Runtime) intlDefaultLocale() intl.Locale {
	if canon, err := r.canonicalizer(); err == nil {
		if l, err := canon.Canonicalize(r.Locale()); err == nil {
			return l
		}
	}
	l, _ := intl.ParseLocale("en-US")
	return l
}

// matcherKind is the localeMatcher option as go-intl takes it.
func matcherKind(matcher string) intl.MatcherKind {
	if matcher == "lookup" {
		return intl.Lookup
	}
	return intl.BestFit
}

// intlLocales reads canonical tags, as requestedLocales gives them, as
// go-intl's locales.
func intlLocales(tags []string) []intl.Locale {
	out := make([]intl.Locale, 0, len(tags))
	for _, tag := range tags {
		if l, err := intl.ParseLocale(tag); err == nil {
			out = append(out, l)
		}
	}
	return out
}

// intlLocale is ECMA-402's ResolveLocale as far as the locale: the first
// requested locale the service is available in, with its Unicode extension,
// or the default locale. The service reads the extension's keywords itself.
func (r *Runtime) intlLocale(service intl.Service, tags []string, matcher string) (intl.Locale, error) {
	m, err := intl.NewLocaleMatcher(intl.Embedded, service)
	if err != nil {
		return intl.Locale{}, r.intlInternal()
	}
	return m.Resolve(intlLocales(tags), matcherKind(matcher), r.intlDefaultLocale()), nil
}

// defSupportedLocalesOfService defines a constructor's supportedLocalesOf
// over a service's available locales.
func (r *Runtime) defSupportedLocalesOfService(ctor *Object, service intl.Service) {
	r.defMethod(ctor, "supportedLocalesOf", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		tags, err := rt.requestedLocales(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		options, err := rt.optionsObject(arg(args, 1))
		if err != nil {
			return Undefined, err
		}
		matcher, err := rt.stringOption(options, "localeMatcher", "best fit", "lookup", "best fit")
		if err != nil {
			return Undefined, err
		}
		m, err := intl.NewLocaleMatcher(intl.Embedded, service)
		if err != nil {
			return Undefined, rt.intlInternal()
		}
		var out []Value
		for _, tag := range tags {
			if len(m.Supported(intlLocales([]string{tag}), matcherKind(matcher))) > 0 {
				out = append(out, Str(NewString(tag)))
			}
		}
		return Obj(rt.newArrayFrom(out)), nil
	})
}

// intlDigits is the digit options a numberOptions settled on, as go-intl
// takes them: the ones the rounding it settled on counts by, so that go-intl
// settles on the same.
type intlDigits struct {
	minInt                           int
	minFrac, maxFrac, minSig, maxSig *int
	priority                         intl.RoundingPriority
	mode                             intl.RoundingMode
	increment                        int
	trailing                         intl.TrailingZeroDisplay
	notation                         intl.Notation
	compact                          intl.CompactDisplay
}

func (o *numberOptions) intlDigits() intlDigits {
	d := intlDigits{minInt: o.minInt, increment: o.roundingIncrement}
	frac := func() { d.minFrac, d.maxFrac = intl.Digits(o.minFrac), intl.Digits(o.maxFrac) }
	sig := func() { d.minSig, d.maxSig = intl.Digits(o.minSig), intl.Digits(o.maxSig) }
	switch o.rounding {
	case "fraction":
		frac()
	case "significant":
		sig()
	case "morePrecision", "lessPrecision":
		if o.roundingPriority != "auto" {
			frac()
			sig()
		}
		// Otherwise it is compact notation with nothing asked of it, which
		// go-intl settles on itself.
	}
	d.priority = map[string]intl.RoundingPriority{"auto": intl.PriorityAuto,
		"morePrecision": intl.MorePrecision, "lessPrecision": intl.LessPrecision}[o.roundingPriority]
	d.mode = map[string]intl.RoundingMode{"ceil": intl.Ceil, "floor": intl.Floor, "expand": intl.Expand,
		"trunc": intl.Trunc, "halfCeil": intl.HalfCeil, "halfFloor": intl.HalfFloor,
		"halfExpand": intl.HalfExpand, "halfTrunc": intl.HalfTrunc, "halfEven": intl.HalfEven}[o.roundingMode]
	if o.trailingZero == "stripIfInteger" {
		d.trailing = intl.TrailingZeroStripIfInteger
	}
	d.notation = map[string]intl.Notation{"standard": intl.NotationStandard, "compact": intl.NotationCompact,
		"scientific": intl.NotationScientific, "engineering": intl.NotationEngineering}[o.notation]
	if o.compactDisplay == "long" {
		d.compact = intl.CompactLong
	}
	return d
}

// intlDecimal is a number as numberArgument read it, as go-intl takes it:
// its exact digits, or NaN or an infinity.
func intlDecimal(d decimal, special string) intl.Decimal {
	switch special {
	case "nan":
		return intl.DecimalFromFloat(math.NaN())
	case "inf":
		return intl.DecimalFromFloat(math.Inf(1))
	case "-inf":
		return intl.DecimalFromFloat(math.Inf(-1))
	}
	text := "0"
	if d.digits != "" {
		text = "0." + d.digits + "e" + strconv.Itoa(d.exp)
	}
	if d.negative {
		text = "-" + text
	}
	return intl.ParseDecimal(text)
}

// intlOptions is a NumberFormat's options, as the runtime settled them, as
// go-intl takes them.
func (o *numberOptions) intlOptions(numbering string, compat intl.Compat) intl.NumberFormatOptions {
	d := o.intlDigits()
	opts := intl.NumberFormatOptions{
		Unit: o.unit, Currency: o.currency, NumberingSystem: numbering, Compat: compat,
		MinimumIntegerDigits: d.minInt, MinimumFractionDigits: d.minFrac,
		MaximumFractionDigits: d.maxFrac, MinimumSignificantDigits: d.minSig,
		MaximumSignificantDigits: d.maxSig, RoundingPriority: d.priority, RoundingMode: d.mode,
		RoundingIncrement: d.increment, TrailingZeroDisplay: d.trailing,
		Notation: d.notation, CompactDisplay: d.compact,
		Style: map[string]intl.Style{"decimal": intl.StyleDecimal, "percent": intl.StylePercent,
			"currency": intl.StyleCurrency, "unit": intl.StyleUnit}[o.style],
		CurrencyDisplay: map[string]intl.CurrencyDisplay{"symbol": intl.CurrencySymbol,
			"narrowSymbol": intl.CurrencyNarrowSymbol, "code": intl.CurrencyCode,
			"name": intl.CurrencyName}[o.currencyDisplay],
		UnitDisplay: map[string]intl.UnitDisplay{"short": intl.UnitShort, "long": intl.UnitLong,
			"narrow": intl.UnitNarrow}[o.unitDisplay],
		SignDisplay: map[string]intl.SignDisplay{"auto": intl.SignAuto, "always": intl.SignAlways,
			"exceptZero": intl.SignExceptZero, "never": intl.SignNever,
			"negative": intl.SignNegative}[o.signDisplay],
		UseGrouping: map[string]intl.Grouping{"": intl.GroupingNever, "auto": intl.GroupingAuto,
			"always": intl.GroupingAlways, "min2": intl.GroupingMin2}[o.useGrouping],
	}
	if o.currencySign == "accounting" {
		opts.CurrencySign = intl.CurrencySignAccounting
	}
	return opts
}
