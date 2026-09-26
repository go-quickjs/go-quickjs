package vm

import (
	"strings"

	"github.com/go-quickjs/go-quickjs/internal/jsnum"
)

// Intl's errors, in V8's words, which is what a script sees from Node: the
// message templates of V8's message-template.h that Intl throws with,
// filled as V8 fills them. go-intl's own errors are Go's, prefixed "intl:",
// and never reach a script.

// enterIntl notes which constructor is reading its options, for the errors
// that name it, and returns what restores the one before, since an option's
// getter may build another formatter.
func (r *Runtime) enterIntl(service string) func() {
	previous := r.intlService
	r.intlService = service
	return func() { r.intlService = previous }
}

// v8Describe is a value as V8's messages print it, without calling into the
// script: an object by its constructor's name, "#<Object>".
func (r *Runtime) v8Describe(v Value) string {
	switch v.Kind() {
	case KindUndefined:
		return "undefined"
	case KindNull:
		return "null"
	case KindString:
		return v.String().Go()
	case KindNumber:
		return jsnum.FormatFloat(v.Number())
	case KindBool:
		if v.BoolValue() {
			return "true"
		}
		return "false"
	case KindSymbol:
		return v.Symbol().String()
	}
	if o := v.Object(); o != nil {
		// A Temporal value, whose tag names it, V8 prints as
		// Object.prototype.toString would: "[object Temporal.PlainDate]".
		for p := o.proto; p != nil; p = p.proto {
			if t := p.getOwnVisible(r.atoms.internSymbol(r.wellKnown.toStringTag)); t != nil {
				if !t.isAccessor() && t.value.IsString() && strings.HasPrefix(t.value.String().Go(), "Temporal.") {
					return "[object " + t.value.String().Go() + "]"
				}
				break
			}
		}
		name := "Object"
		// The constructor named by the nearest prototype that has one, read
		// as data only, so that no getter runs.
		for p := o.proto; p != nil; p = p.proto {
			c := p.getOwnVisible(r.atoms.intern("constructor"))
			if c == nil {
				continue
			}
			if fn := c.value.Object(); fn != nil && !c.isAccessor() {
				if n := fn.getOwnVisible(r.atoms.intern("name")); n != nil && !n.isAccessor() &&
					n.value.IsString() && n.value.String().Go() != "" {
					name = n.value.String().Go()
				}
			}
			break
		}
		return "#<" + name + ">"
	}
	return v.Kind().String()
}

// kIncompatibleMethodReceiver.
func (r *Runtime) intlIncompatibleReceiver(method string, this Value) error {
	return r.throwTypeError("Method %s called on incompatible receiver %s", method, r.v8Describe(this))
}

// kConstructorNotFunction.
func (r *Runtime) intlRequiresNew(ctor string) error {
	return r.throwTypeError("Constructor %s requires 'new'", ctor)
}

// kValueOutOfRange: an option that is not one of the words it may be.
func (r *Runtime) intlValueOutOfRange(value, property string) error {
	return r.throwRangeError("Value %s out of range for %s options property %s", value, r.intlService, property)
}

// kPropertyValueOutOfRange: a number option outside its bounds.
func (r *Runtime) intlPropertyOutOfRange(property string) error {
	return r.throwRangeError("%s value is out of range.", property)
}

// kInvalid: "Invalid calendar : x!", as a RangeError or a TypeError.
func (r *Runtime) intlInvalidRange(what, value string) error {
	return r.throwRangeError("Invalid %s : %s", what, value)
}

func (r *Runtime) intlInvalidType(what, value string) error {
	return r.throwTypeError("Invalid %s : %s", what, value)
}

// kInvalidLanguageTag.
func (r *Runtime) intlInvalidLanguageTag(tag string) error {
	return r.throwRangeError("Invalid language tag: %s", tag)
}

// kLocaleNotEmpty and kLocaleBadParameters, Intl.Locale's.
func (r *Runtime) intlLocaleEmpty(typeError bool) error {
	const message = "First argument to Intl.Locale constructor can't be empty or missing"
	if typeError {
		return r.throwTypeError(message)
	}
	return r.throwRangeError(message)
}

func (r *Runtime) intlIncorrectLocale() error {
	return r.throwRangeError("Incorrect locale information provided")
}

// kInvalidArgument, which V8 words as its template's name.
func (r *Runtime) intlInvalidArgumentType() error  { return r.throwTypeError("invalid_argument") }
func (r *Runtime) intlInvalidArgumentRange() error { return r.throwRangeError("invalid_argument") }

// kInvalidTimeValue.
func (r *Runtime) intlInvalidTimeValue(typeError bool) error {
	if typeError {
		return r.throwTypeError("Invalid time value")
	}
	return r.throwRangeError("Invalid time value")
}

// kIcuError, for what go-intl refuses that the runtime's own checks of the
// options should already have refused.
func (r *Runtime) intlInternal() error {
	return r.throwRangeError("Internal error. Icu error.")
}

// intlNullOptions is ToObject of null options, as V8 words it for Intl's
// older constructors.
func (r *Runtime) intlNullOptions() error {
	return r.throwTypeError("%s called on null or undefined", r.intlService)
}

// intlTemporal is the words of a Temporal error, as temporal_rs gives them
// and V8 passes them on.
func (r *Runtime) intlTemporalRange(message string) error {
	return r.throwRangeError("Temporal error: %s", message)
}

func (r *Runtime) intlTemporalType(message string) error {
	return r.throwTypeError("Temporal error: %s", message)
}
