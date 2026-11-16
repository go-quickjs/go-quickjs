// Package regdiff holds the register interpreter's differential tests and
// nothing else: programs -- cases of their own, the micro-benchmarks and
// the V8 suite, found through internal/v8bench -- run with the register
// tier off and on and every answer compared (docs/register-interpreter.md,
// "Raw int32 registers"); the same corpus run by the native handlers and
// by the Go table, their inline caches compared (TestRegNativeCaches); and,
// in JIT builds, each function's register code lowered and evaluated
// (TestRegLowerEvaluate).
package regdiff
