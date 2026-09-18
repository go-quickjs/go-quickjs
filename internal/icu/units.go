package icu

import "sort"

// The units a number may be written in.
//
// Not every unit in the world: the ones ECMA-402 sanctions, which is a list of
// forty-five chosen because every language has a name for each of them. A unit
// may also be one of these divided by another -- kilometre per hour, litre per
// hundred kilometres -- which is why they are a set rather than a table.
var sanctionedUnits = map[string]bool{
	"acre": true, "bit": true, "byte": true, "celsius": true,
	"centimeter": true, "day": true, "degree": true, "fahrenheit": true,
	"fluid-ounce": true, "foot": true, "gallon": true, "gigabit": true,
	"gigabyte": true, "gram": true, "hectare": true, "hour": true,
	"inch": true, "kilobit": true, "kilobyte": true, "kilogram": true,
	"kilometer": true, "liter": true, "megabit": true, "megabyte": true,
	"meter": true, "microsecond": true, "mile": true, "mile-scandinavian": true,
	"milliliter": true, "millimeter": true, "millisecond": true, "minute": true,
	"month": true, "nanosecond": true, "ounce": true, "percent": true,
	"petabyte": true, "pound": true, "second": true, "stone": true,
	"terabit": true, "terabyte": true, "week": true, "yard": true, "year": true,
}

// HasUnit reports whether a unit is one a number may be written in.
func HasUnit(name string) bool { return sanctionedUnits[name] }

// Units lists them, in order.
func Units() []string {
	out := make([]string, 0, len(sanctionedUnits))
	for name := range sanctionedUnits {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
