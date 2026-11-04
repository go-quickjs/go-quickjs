package vm

import "math/bits"

// This file sorts after vm.go and before vm_call.go, where placements'
// second pad goes: what is added here moves no hot function against the
// ones it is placed with.

// mathClz32Of is Math.clz32 of a number, unaryMath's last function
// (mathClz32). It is a function rather than a closure in unaryMath's
// initializer, which is laid out at the end of the package's closures: at a
// distance from the tree tier's hot closures that the size of every closure
// between them decides and no placement varies, where 32 bytes one way or
// the other made math_clz32 1.0 or 1.27 times as long. Its number is
// truncated by toInt32, inlined, rather than two calls.
func mathClz32Of(f float64) float64 { return float64(bits.LeadingZeros32(uint32(toInt32(f)))) }
