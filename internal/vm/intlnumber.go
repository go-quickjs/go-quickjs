package vm

import (
	intl "github.com/go-quickjs/go-intl"
)

// numberOptions is a resolved Intl.NumberFormat: the options as the runtime
// read and settled them, which resolvedOptions reports, and the go-intl
// formatter they built, which Intl.NumberFormat and the toLocaleString
// methods defined in terms of it write with.
type numberOptions struct {
	style           string // decimal, percent, currency, unit
	currency        string
	currencyDisplay string
	currencySign    string // standard, accounting
	unit            string
	unitDisplay     string
	notation        string // standard, compact, scientific, engineering
	compactDisplay  string
	signDisplay     string // auto, always, never, exceptZero, negative
	// useGrouping is a word rather than a flag: always, auto, min2, or empty
	// for no grouping at all.
	useGrouping string

	minInt           int
	minFrac, maxFrac int
	// minSig and maxSig are the significant-digit counts, which take the place
	// of the fraction counts when they are given.
	minSig, maxSig int
	// rounding is which of the two counts is in force: fraction, significant,
	// or one of the two that asks for both and keeps whichever says more.
	rounding string
	// reportSig and reportFrac say which of the two counts resolvedOptions
	// tells of, which is not always the one the rounding went by.
	reportSig, reportFrac bool
	roundingMode          string
	roundingPriority      string
	roundingIncrement     int
	trailingZero          string

	// formatFn is the bound function the format getter hands out, kept so that
	// every ask answers with the same one.
	formatFn *Object

	nf *intl.NumberFormat
}
