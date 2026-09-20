package vm

import (
	"math/big"
	"strings"
)

var temporalUnitNanoseconds = map[string]int64{
	"hour": 3_600_000_000_000, "minute": 60_000_000_000,
	"second": 1_000_000_000, "millisecond": 1_000_000,
	"microsecond": 1_000, "nanosecond": 1,
}

var temporalUnitRank = map[string]int{
	"hour": 0, "minute": 1, "second": 2, "millisecond": 3,
	"microsecond": 4, "nanosecond": 5,
}

func (r *Runtime) initTemporalInstantOperations(proto *Object) {
	for _, operation := range []struct {
		name  string
		since bool
	}{{"until", false}, {"since", true}} {
		op := operation
		r.defMethod(proto, op.name, 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
			instant, err := rt.temporalInstantValue(this, "Temporal.Instant.prototype."+op.name)
			if err != nil {
				return Undefined, err
			}
			other, err := rt.toTemporalInstant(arg(args, 0))
			if err != nil {
				return Undefined, err
			}
			largest, smallest, increment, mode, err := rt.temporalDifferenceOptions(arg(args, 1), "second")
			if err != nil {
				return Undefined, err
			}
			difference := new(big.Int).Sub(other.epochNanoseconds(), instant.epochNanoseconds())
			if op.since {
				difference.Neg(difference)
			}
			step := new(big.Int).Mul(big.NewInt(temporalUnitNanoseconds[smallest]), big.NewInt(increment))
			difference = roundTemporalBigInt(difference, step, mode)
			duration := temporalDurationFromNanoseconds(difference, largest)
			return Obj(newTemporalDuration(rt.temporalDurationProto, duration)), nil
		})
	}

	r.defMethod(proto, "round", 1, func(rt *Runtime, this Value, args []Value) (Value, error) {
		instant, err := rt.temporalInstantValue(this, "Temporal.Instant.prototype.round")
		if err != nil {
			return Undefined, err
		}
		smallest, increment, mode, err := rt.temporalRoundOptions(arg(args, 0), true, false)
		if err != nil {
			return Undefined, err
		}
		step := new(big.Int).Mul(big.NewInt(temporalUnitNanoseconds[smallest]), big.NewInt(increment))
		rounded := roundTemporalBigIntAsIfPositive(instant.epochNanoseconds(), step, mode)
		result, ok := temporalInstantFromEpochNanoseconds(rounded)
		if !ok {
			return Undefined, rt.throwRangeError("rounded instant is outside the Temporal range")
		}
		return Obj(newTemporalInstant(proto, result)), nil
	})
}

func normalizeTemporalUnit(unit string) (string, bool) {
	if strings.HasSuffix(unit, "s") {
		unit = strings.TrimSuffix(unit, "s")
	}
	_, ok := temporalUnitNanoseconds[unit]
	return unit, ok
}

func (r *Runtime) temporalDifferenceOptions(value Value, defaultLargest string) (largest, smallest string, increment int64, mode string, err error) {
	options, err := r.strictOptions(value)
	if err != nil {
		return "", "", 0, "", err
	}
	largestRaw, err := r.stringOption(options, "largestUnit", "auto")
	if err != nil {
		return "", "", 0, "", err
	}
	rawIncrement, incrementSet, err := r.rawNumberOption(options, "roundingIncrement")
	if err != nil {
		return "", "", 0, "", err
	}
	mode, err = r.stringOption(options, "roundingMode", "trunc",
		"ceil", "floor", "expand", "trunc", "halfCeil", "halfFloor", "halfExpand", "halfTrunc", "halfEven")
	if err != nil {
		return "", "", 0, "", err
	}
	smallestRaw, err := r.stringOption(options, "smallestUnit", "nanosecond")
	if err != nil {
		return "", "", 0, "", err
	}
	smallest, ok := normalizeTemporalUnit(smallestRaw)
	if !ok {
		return "", "", 0, "", r.throwRangeError("invalid smallestUnit")
	}
	if largestRaw == "auto" {
		largestRaw = defaultLargest
		if temporalUnitRank[smallest] < temporalUnitRank[largestRaw] {
			largestRaw = smallest
		}
	}
	largest, ok = normalizeTemporalUnit(largestRaw)
	if !ok {
		return "", "", 0, "", r.throwRangeError("invalid largestUnit")
	}
	if temporalUnitRank[largest] > temporalUnitRank[smallest] {
		return "", "", 0, "", r.throwRangeError("largestUnit must not be smaller than smallestUnit")
	}
	increment, err = r.validateTemporalRoundingIncrement(rawIncrement, incrementSet, smallest, false)
	if err != nil {
		return "", "", 0, "", err
	}
	return
}

func (r *Runtime) temporalRoundOptions(value Value, dayDividend, allowDay bool) (smallest string, increment int64, mode string, err error) {
	var options *Object
	if value.IsString() {
		options = newObject(nil, ClassObject)
		options.setOwnRaw(r.atoms.intern("smallestUnit"), value, propDefault)
	} else {
		if value.IsUndefined() {
			return "", 0, "", r.throwTypeError("options or a smallestUnit string is required")
		}
		options, err = r.strictOptions(value)
		if err != nil {
			return "", 0, "", err
		}
	}
	rawIncrement, incrementSet, err := r.rawNumberOption(options, "roundingIncrement")
	if err != nil {
		return "", 0, "", err
	}
	mode, err = r.stringOption(options, "roundingMode", "halfExpand",
		"ceil", "floor", "expand", "trunc", "halfCeil", "halfFloor", "halfExpand", "halfTrunc", "halfEven")
	if err != nil {
		return "", 0, "", err
	}
	raw, err := r.stringOption(options, "smallestUnit", "")
	if err != nil {
		return "", 0, "", err
	}
	var ok bool
	smallest, ok = normalizeTemporalUnit(raw)
	if !ok && allowDay && strings.TrimSuffix(raw, "s") == "day" {
		smallest, ok = "day", true
	}
	if !ok {
		return "", 0, "", r.throwRangeError("smallestUnit is required")
	}
	increment, err = r.validateTemporalRoundingIncrement(rawIncrement, incrementSet, smallest, dayDividend)
	if err != nil {
		return "", 0, "", err
	}
	return
}

func (r *Runtime) validateTemporalRoundingIncrement(n float64, set bool, unit string, dayDividend bool) (int64, error) {
	if !set {
		return 1, nil
	}
	n = float64(int64(n))
	if n < 1 || n > 1_000_000_000 {
		return 0, r.throwRangeError("roundingIncrement must be a positive integer")
	}
	increment := int64(n)
	if unit == "day" {
		if increment != 1 {
			return 0, r.throwRangeError("roundingIncrement does not divide the next largest unit")
		}
		return increment, nil
	}
	maximum := int64(1000)
	if dayDividend {
		maximum = temporalSecondsPerDay * temporalNanosecondsPerSecond / temporalUnitNanoseconds[unit]
	} else {
		switch unit {
		case "hour":
			maximum = 24
		case "minute", "second":
			maximum = 60
		}
	}
	tooLarge := increment >= maximum
	if dayDividend {
		tooLarge = increment > maximum
	}
	if tooLarge || maximum%increment != 0 {
		return 0, r.throwRangeError("roundingIncrement does not divide the next largest unit")
	}
	return increment, nil
}

func roundTemporalBigInt(value, increment *big.Int, mode string) *big.Int {
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(value, increment, remainder)
	if remainder.Sign() == 0 {
		return quotient.Mul(quotient, increment)
	}
	sign := value.Sign()
	adjust := false
	switch mode {
	case "expand":
		adjust = true
	case "ceil":
		adjust = sign > 0
	case "floor":
		adjust = sign < 0
	case "trunc":
	case "halfCeil", "halfFloor", "halfExpand", "halfTrunc", "halfEven":
		twice := new(big.Int).Lsh(new(big.Int).Abs(remainder), 1)
		comparison := twice.Cmp(increment)
		if comparison > 0 {
			adjust = true
		} else if comparison == 0 {
			switch mode {
			case "halfCeil":
				adjust = sign > 0
			case "halfFloor":
				adjust = sign < 0
			case "halfExpand":
				adjust = true
			case "halfEven":
				adjust = quotient.Bit(0) == 1
			}
		}
	}
	if adjust {
		quotient.Add(quotient, big.NewInt(int64(sign)))
	}
	return quotient.Mul(quotient, increment)
}

func roundTemporalBigIntAsIfPositive(value, increment *big.Int, mode string) *big.Int {
	lower, remainder := new(big.Int), new(big.Int)
	lower.DivMod(value, increment, remainder)
	chooseUpper := false
	switch mode {
	case "ceil", "expand":
		chooseUpper = remainder.Sign() != 0
	case "floor", "trunc":
	case "halfCeil", "halfFloor", "halfExpand", "halfTrunc", "halfEven":
		comparison := new(big.Int).Lsh(new(big.Int).Set(remainder), 1).Cmp(increment)
		if comparison > 0 {
			chooseUpper = true
		} else if comparison == 0 {
			switch mode {
			case "halfCeil", "halfExpand":
				chooseUpper = true
			case "halfEven":
				chooseUpper = lower.Bit(0) == 1
			}
		}
	}
	if chooseUpper {
		lower.Add(lower, big.NewInt(1))
	}
	return lower.Mul(lower, increment)
}

func temporalDurationFromNanoseconds(value *big.Int, largest string) temporalDuration {
	var duration temporalDuration
	remainder := new(big.Int).Set(value)
	if largest == "day" {
		dayNanoseconds := big.NewInt(temporalSecondsPerDay * temporalNanosecondsPerSecond)
		days := new(big.Int)
		days.QuoRem(remainder, dayNanoseconds, remainder)
		if days.Sign() != 0 {
			duration.days, _ = new(big.Float).SetInt(days).Float64()
		}
		largest = "hour"
	}
	fields := []struct {
		unit   string
		target *float64
	}{
		{"hour", &duration.hours}, {"minute", &duration.minutes}, {"second", &duration.seconds},
		{"millisecond", &duration.milliseconds}, {"microsecond", &duration.microseconds}, {"nanosecond", &duration.nanoseconds},
	}
	start := temporalUnitRank[largest]
	for i := start; i < len(fields); i++ {
		scale := big.NewInt(temporalUnitNanoseconds[fields[i].unit])
		quotient := new(big.Int)
		quotient.QuoRem(remainder, scale, remainder)
		if quotient.Sign() != 0 {
			value, _ := new(big.Float).SetInt(quotient).Float64()
			*fields[i].target = value
		}
	}
	return duration
}
