package api

import "strings"

// CoreFeatures is a set of WebAssembly Core specification features, one bit
// each.
//
// The engine implements WebAssembly 3.0 entire, with the threads proposal,
// and none of it can be turned off: the set a RuntimeConfig is given records
// what the embedder asked for and is reported back by it, but every module
// is decoded and validated with every feature on. A module that uses a
// feature outside the set still compiles.
type CoreFeatures uint64

// CoreFeaturesV1 are the features of WebAssembly 1.0 (20191205).
const CoreFeaturesV1 = CoreFeatureMutableGlobal

// CoreFeaturesV2 are the features of WebAssembly 2.0.
const CoreFeaturesV2 = CoreFeaturesV1 |
	CoreFeatureBulkMemoryOperations |
	CoreFeatureMultiValue |
	CoreFeatureNonTrappingFloatToIntConversion |
	CoreFeatureReferenceTypes |
	CoreFeatureSignExtensionOps |
	CoreFeatureSIMD

// CoreFeaturesV3 are the features of WebAssembly 3.0.
const CoreFeaturesV3 = CoreFeaturesV2 |
	CoreFeatureTailCall |
	CoreFeatureExtendedConst |
	CoreFeatureMultiMemory |
	CoreFeatureMemory64 |
	CoreFeatureExceptionHandling |
	CoreFeatureFunctionReferences |
	CoreFeatureGC |
	CoreFeatureRelaxedSIMD

// CoreFeaturesAll are the features the engine implements: WebAssembly 3.0
// and the threads proposal. It is what a RuntimeConfig reports by default.
const CoreFeaturesAll = CoreFeaturesV3 | CoreFeatureThreads

// The features, one bit each.
const (
	// CoreFeatureBulkMemoryOperations adds memory.copy, memory.fill,
	// table.copy and the segment instructions.
	CoreFeatureBulkMemoryOperations CoreFeatures = 1 << iota
	// CoreFeatureMultiValue allows several results from functions and
	// blocks.
	CoreFeatureMultiValue
	// CoreFeatureMutableGlobal allows importing and exporting mutable
	// globals.
	CoreFeatureMutableGlobal
	// CoreFeatureNonTrappingFloatToIntConversion adds the saturating
	// truncations.
	CoreFeatureNonTrappingFloatToIntConversion
	// CoreFeatureReferenceTypes adds funcref and externref values and
	// several tables.
	CoreFeatureReferenceTypes
	// CoreFeatureSignExtensionOps adds the sign-extension instructions.
	CoreFeatureSignExtensionOps
	// CoreFeatureSIMD adds v128 and the vector instructions.
	CoreFeatureSIMD
	// CoreFeatureTailCall adds return_call and return_call_indirect.
	CoreFeatureTailCall
	// CoreFeatureExtendedConst allows arithmetic in constant expressions.
	CoreFeatureExtendedConst
	// CoreFeatureMultiMemory allows several memories.
	CoreFeatureMultiMemory
	// CoreFeatureMemory64 adds memories and tables with i64 addresses.
	CoreFeatureMemory64
	// CoreFeatureExceptionHandling adds tags, throw and try_table.
	CoreFeatureExceptionHandling
	// CoreFeatureFunctionReferences adds typed function references.
	CoreFeatureFunctionReferences
	// CoreFeatureGC adds structs, arrays, i31 and subtyping.
	CoreFeatureGC
	// CoreFeatureRelaxedSIMD adds the relaxed vector instructions.
	CoreFeatureRelaxedSIMD
	// CoreFeatureThreads adds shared memories and atomic instructions.
	CoreFeatureThreads
)

// SetEnabled returns f with feature turned on or off.
func (f CoreFeatures) SetEnabled(feature CoreFeatures, val bool) CoreFeatures {
	if val {
		return f | feature
	}
	return f &^ feature
}

// IsEnabled reports whether any feature of feature is in f.
func (f CoreFeatures) IsEnabled(feature CoreFeatures) bool {
	return f&feature != 0
}

// RequireEnabled returns an error naming feature if it is not in f.
func (f CoreFeatures) RequireEnabled(feature CoreFeatures) error {
	if !f.IsEnabled(feature) {
		return &featureError{feature}
	}
	return nil
}

type featureError struct{ f CoreFeatures }

func (e *featureError) Error() string { return "feature \"" + e.f.String() + "\" is disabled" }

// String returns the names of the features in f, joined by '|'.
func (f CoreFeatures) String() string {
	var b strings.Builder
	for i := 0; i < 64; i++ {
		bit := CoreFeatures(1) << i
		if f&bit == 0 {
			continue
		}
		if name := featureName(bit); name != "" {
			if b.Len() > 0 {
				b.WriteByte('|')
			}
			b.WriteString(name)
		}
	}
	return b.String()
}

func featureName(f CoreFeatures) string {
	switch f {
	case CoreFeatureMutableGlobal:
		return "mutable-global"
	case CoreFeatureSignExtensionOps:
		return "sign-extension-ops"
	case CoreFeatureMultiValue:
		return "multi-value"
	case CoreFeatureNonTrappingFloatToIntConversion:
		return "nontrapping-float-to-int-conversion"
	case CoreFeatureBulkMemoryOperations:
		return "bulk-memory-operations"
	case CoreFeatureReferenceTypes:
		return "reference-types"
	case CoreFeatureSIMD:
		return "simd"
	case CoreFeatureTailCall:
		return "tail-call"
	case CoreFeatureExtendedConst:
		return "extended-const"
	case CoreFeatureMultiMemory:
		return "multi-memory"
	case CoreFeatureMemory64:
		return "memory64"
	case CoreFeatureExceptionHandling:
		return "exception-handling"
	case CoreFeatureFunctionReferences:
		return "function-references"
	case CoreFeatureGC:
		return "gc"
	case CoreFeatureRelaxedSIMD:
		return "relaxed-simd"
	case CoreFeatureThreads:
		return "threads"
	}
	return ""
}
