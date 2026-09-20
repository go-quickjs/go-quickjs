package vm

import (
	"math"
	"math/big"
	"strconv"
	"strings"
)

type temporalDuration struct {
	years, months, weeks, days              float64
	hours, minutes, seconds                 float64
	milliseconds, microseconds, nanoseconds float64
}

func (d temporalDuration) fields() [10]float64 {
	return [10]float64{d.years, d.months, d.weeks, d.days, d.hours, d.minutes, d.seconds, d.milliseconds, d.microseconds, d.nanoseconds}
}

func (d temporalDuration) sign() int {
	for _, value := range d.fields() {
		if value < 0 {
			return -1
		}
		if value > 0 {
			return 1
		}
	}
	return 0
}

func (d temporalDuration) valid() bool {
	sign := 0
	for _, value := range d.fields() {
		if value == 0 {
			continue
		}
		current := 1
		if value < 0 {
			current = -1
		}
		if sign != 0 && sign != current {
			return false
		}
		sign = current
	}
	return true
}

func (d temporalDuration) timeNanoseconds() (*big.Int, bool) {
	if d.years != 0 || d.months != 0 || d.weeks != 0 || d.days != 0 {
		return nil, false
	}
	total := new(big.Int)
	for _, unit := range []struct {
		value float64
		scale int64
	}{
		{d.hours, 3_600_000_000_000}, {d.minutes, 60_000_000_000}, {d.seconds, 1_000_000_000},
		{d.milliseconds, 1_000_000}, {d.microseconds, 1_000}, {d.nanoseconds, 1},
	} {
		integer, _ := new(big.Float).SetFloat64(unit.value).Int(nil)
		total.Add(total, new(big.Int).Mul(integer, big.NewInt(unit.scale)))
	}
	return total, true
}

func (r *Runtime) initTemporalDuration(temporal *Object) {
	proto := newObject(r.proto.object, ClassObject)
	r.temporalDurationProto = proto
	r.newTemporalCtor(temporal, "Duration", 0, proto, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if err := rt.requireNew("Temporal.Duration"); err != nil {
			return Undefined, err
		}
		var d temporalDuration
		fields := []*float64{&d.years, &d.months, &d.weeks, &d.days, &d.hours, &d.minutes, &d.seconds, &d.milliseconds, &d.microseconds, &d.nanoseconds}
		for i, target := range fields {
			value, err := rt.temporalInteger(arg(args, i))
			if err != nil {
				return Undefined, err
			}
			if value > 9_007_199_254_740_991 || value < -9_007_199_254_740_991 {
				return Undefined, rt.throwRangeError("Duration constructor fields must be safe integers")
			}
			*target = value
		}
		if !d.valid() {
			return Undefined, rt.throwRangeError("duration fields must have the same sign")
		}
		instanceProto, err := rt.protoFromNewTargetErr(proto)
		if err != nil {
			return Undefined, err
		}
		return Obj(newTemporalDuration(instanceProto, d)), nil
	})
	ctor, _ := r.getProp(temporal, r.atoms.intern("Duration"), Obj(temporal))
	r.defMethod(ctor.Object(), "from", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		d, err := rt.toTemporalDuration(arg(args, 0))
		if err != nil {
			return Undefined, err
		}
		return Obj(newTemporalDuration(proto, d)), nil
	})
	for i, name := range []string{"years", "months", "weeks", "days", "hours", "minutes", "seconds", "milliseconds", "microseconds", "nanoseconds"} {
		index, property := i, name
		r.defGetter(proto, property, func(rt *Runtime, this Value, args []Value) (Value, error) {
			d, err := rt.temporalDurationValue(this, "get Temporal.Duration.prototype."+property)
			if err != nil {
				return Undefined, err
			}
			return Float(d.fields()[index]), nil
		})
	}
	r.defGetter(proto, "sign", func(rt *Runtime, this Value, args []Value) (Value, error) {
		d, err := rt.temporalDurationValue(this, "get Temporal.Duration.prototype.sign")
		if err != nil {
			return Undefined, err
		}
		return Int(d.sign()), nil
	})
	r.defGetter(proto, "blank", func(rt *Runtime, this Value, args []Value) (Value, error) {
		d, err := rt.temporalDurationValue(this, "get Temporal.Duration.prototype.blank")
		if err != nil {
			return Undefined, err
		}
		return Bool(d.sign() == 0), nil
	})
	r.defMethod(proto, "negated", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		d, err := rt.temporalDurationValue(this, "Temporal.Duration.prototype.negated")
		if err != nil {
			return Undefined, err
		}
		values := d.fields()
		for i := range values {
			values[i] = -values[i]
		}
		return Obj(newTemporalDuration(proto, durationFromFields(values))), nil
	})
	r.defMethod(proto, "abs", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		d, err := rt.temporalDurationValue(this, "Temporal.Duration.prototype.abs")
		if err != nil {
			return Undefined, err
		}
		values := d.fields()
		for i := range values {
			if values[i] < 0 {
				values[i] = -values[i]
			}
		}
		return Obj(newTemporalDuration(proto, durationFromFields(values))), nil
	})
	for _, name := range []string{"toString", "toJSON"} {
		method := name
		r.defMethod(proto, method, 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
			d, err := rt.temporalDurationValue(this, "Temporal.Duration.prototype."+method)
			if err != nil {
				return Undefined, err
			}
			return Str(NewString(d.string())), nil
		})
	}
	r.defMethod(proto, "valueOf", 0, func(rt *Runtime, this Value, args []Value) (Value, error) {
		if _, err := rt.temporalDurationValue(this, "Temporal.Duration.prototype.valueOf"); err != nil {
			return Undefined, err
		}
		return Undefined, rt.throwTypeError("durations cannot be converted to primitive values")
	})
	r.defToStringTag(proto, "Temporal.Duration")
}

func durationFromFields(v [10]float64) temporalDuration {
	return temporalDuration{v[0], v[1], v[2], v[3], v[4], v[5], v[6], v[7], v[8], v[9]}
}
func newTemporalDuration(proto *Object, d temporalDuration) *Object {
	o := newObject(proto, ClassObject)
	o.data = &d
	return o
}
func (r *Runtime) temporalDurationValue(v Value, method string) (temporalDuration, error) {
	if v.IsObject() {
		if d, ok := v.Object().data.(*temporalDuration); ok {
			return *d, nil
		}
	}
	return temporalDuration{}, r.throwTypeError("%s called on an incompatible receiver", method)
}
func (r *Runtime) temporalInteger(v Value) (float64, error) {
	if v.IsUndefined() {
		return 0, nil
	}
	n, err := r.toNumber(v)
	if err != nil {
		return 0, err
	}
	if math.IsNaN(n) || math.IsInf(n, 0) || n != math.Trunc(n) {
		return 0, r.throwRangeError("duration fields must be finite integers")
	}
	return n, nil
}

func (r *Runtime) toTemporalDuration(v Value) (temporalDuration, error) {
	if v.IsObject() {
		if d, ok := v.Object().data.(*temporalDuration); ok {
			return *d, nil
		}
		return r.temporalDurationFromBag(v.Object())
	}
	if v.IsString() {
		d, err := parseTemporalDuration(v.String().Go())
		if err != nil {
			return temporalDuration{}, r.throwRangeError("invalid Temporal.Duration string")
		}
		return d, nil
	}
	return temporalDuration{}, r.throwTypeError("a duration must be a string or object")
}

func (r *Runtime) temporalDurationFromBag(o *Object) (temporalDuration, error) {
	var d temporalDuration
	found := false
	for _, field := range []struct {
		name   string
		target *float64
	}{
		{"days", &d.days}, {"hours", &d.hours}, {"microseconds", &d.microseconds}, {"milliseconds", &d.milliseconds}, {"minutes", &d.minutes}, {"months", &d.months}, {"nanoseconds", &d.nanoseconds}, {"seconds", &d.seconds}, {"weeks", &d.weeks}, {"years", &d.years},
	} {
		raw, err := r.getProp(o, r.atoms.intern(field.name), Obj(o))
		if err != nil {
			return d, err
		}
		if raw.IsUndefined() {
			continue
		}
		found = true
		value, err := r.temporalInteger(raw)
		if err != nil {
			return d, err
		}
		*field.target = value
	}
	if !found {
		return d, r.throwTypeError("duration property bag has no duration fields")
	}
	if !d.valid() {
		return d, r.throwRangeError("duration fields must have the same sign")
	}
	return d, nil
}

func parseTemporalDuration(s string) (temporalDuration, error) {
	var d temporalDuration
	sign := float64(1)
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		if s[i] == '-' {
			sign = -1
		}
		i++
	}
	if i >= len(s) || s[i] != 'P' {
		return d, errInvalidTemporalInstant
	}
	i++
	seen := false
	inTime := false
	for i < len(s) {
		if s[i] == 'T' && !inTime {
			inTime = true
			i++
			continue
		}
		start := i
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if start == i {
			return d, errInvalidTemporalInstant
		}
		n, err := strconv.ParseFloat(s[start:i], 64)
		if err != nil || math.IsInf(n, 0) || n < 0 || n != math.Trunc(n) {
			return d, errInvalidTemporalInstant
		}
		fraction := ""
		if i < len(s) && (s[i] == '.' || s[i] == ',') {
			i++
			fractionStart := i
			for i < len(s) && s[i] >= '0' && s[i] <= '9' {
				i++
			}
			if fractionStart == i {
				return d, errInvalidTemporalInstant
			}
			fraction = s[fractionStart:i]
		}
		if i >= len(s) {
			return d, errInvalidTemporalInstant
		}
		unit := s[i]
		i++
		target := (*float64)(nil)
		switch unit {
		case 'Y':
			if inTime {
				return d, errInvalidTemporalInstant
			}
			target = &d.years
		case 'M':
			if inTime {
				target = &d.minutes
			} else {
				target = &d.months
			}
		case 'W':
			if inTime {
				return d, errInvalidTemporalInstant
			}
			target = &d.weeks
		case 'D':
			if inTime {
				return d, errInvalidTemporalInstant
			}
			target = &d.days
		case 'H':
			if !inTime {
				return d, errInvalidTemporalInstant
			}
			target = &d.hours
		case 'S':
			if !inTime {
				return d, errInvalidTemporalInstant
			}
			target = &d.seconds
		default:
			return d, errInvalidTemporalInstant
		}
		*target = sign * n
		if fraction != "" {
			if !inTime || i != len(s) {
				return d, errInvalidTemporalInstant
			}
			var scale int64
			switch unit {
			case 'H':
				scale = 3_600_000_000_000
			case 'M':
				scale = 60_000_000_000
			case 'S':
				scale = 1_000_000_000
			default:
				return d, errInvalidTemporalInstant
			}
			extra, ok := roundedFractionNanoseconds(fraction, scale)
			if !ok {
				return d, errInvalidTemporalInstant
			}
			d.addFractionalTime(sign * float64(extra))
		}
		seen = true
	}
	if !seen {
		return d, errInvalidTemporalInstant
	}
	return d, nil
}

func roundedFractionNanoseconds(digits string, scale int64) (int64, bool) {
	numerator, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		return 0, false
	}
	numerator.Mul(numerator, big.NewInt(scale))
	denominator := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(len(digits))), nil)
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(numerator, denominator, remainder)
	if new(big.Int).Lsh(remainder, 1).Cmp(denominator) >= 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if !quotient.IsInt64() {
		return 0, false
	}
	return quotient.Int64(), true
}

func (d *temporalDuration) addFractionalTime(nanoseconds float64) {
	d.minutes += math.Trunc(nanoseconds / 60_000_000_000)
	nanoseconds = math.Mod(nanoseconds, 60_000_000_000)
	d.seconds += math.Trunc(nanoseconds / 1_000_000_000)
	nanoseconds = math.Mod(nanoseconds, 1_000_000_000)
	d.milliseconds += math.Trunc(nanoseconds / 1_000_000)
	nanoseconds = math.Mod(nanoseconds, 1_000_000)
	d.microseconds += math.Trunc(nanoseconds / 1_000)
	d.nanoseconds += math.Mod(nanoseconds, 1_000)
}

func (d temporalDuration) string() string {
	sign := d.sign()
	if sign == 0 {
		return "PT0S"
	}
	values := d.fields()
	if sign < 0 {
		for i := range values {
			values[i] = -values[i]
		}
	}
	var b strings.Builder
	if sign < 0 {
		b.WriteByte('-')
	}
	b.WriteByte('P')
	labels := []byte{'Y', 'M', 'W', 'D'}
	for i := 0; i < 4; i++ {
		if values[i] != 0 {
			b.WriteString(formatTemporalInteger(values[i]))
			b.WriteByte(labels[i])
		}
	}
	if values[4] != 0 || values[5] != 0 || values[6] != 0 || values[7] != 0 || values[8] != 0 || values[9] != 0 {
		b.WriteByte('T')
		if values[4] != 0 {
			b.WriteString(formatTemporalInteger(values[4]))
			b.WriteByte('H')
		}
		if values[5] != 0 {
			b.WriteString(formatTemporalInteger(values[5]))
			b.WriteByte('M')
		}
		sub := values[7]*1_000_000 + values[8]*1_000 + values[9]
		if values[6] != 0 || sub != 0 {
			b.WriteString(formatTemporalInteger(values[6]))
			if sub != 0 {
				fraction := strconv.FormatInt(int64(sub)+1_000_000_000, 10)[1:]
				b.WriteByte('.')
				b.WriteString(strings.TrimRight(fraction, "0"))
			}
			b.WriteByte('S')
		}
	}
	return b.String()
}

func formatTemporalInteger(value float64) string {
	return strconv.FormatFloat(value, 'f', 0, 64)
}
