module github.com/go-quickjs/go-quickjs/internal/cmd/v8bench/goja

go 1.26.0

// The benchmark harness is go-quickjs's own, from the checkout this module
// is in.
replace github.com/go-quickjs/go-quickjs => ../../../..

require (
	github.com/dop251/goja v0.0.0-20261102223426-a177d211bf60
	github.com/go-quickjs/go-quickjs v0.0.0-00010101000000-000000000000
)

require (
	github.com/dlclark/regexp2/v2 v2.11.0 // indirect
	github.com/go-sourcemap/sourcemap v2.1.3+incompatible // indirect
	github.com/google/pprof v0.0.0-20230207041349-798e818bf904 // indirect
	golang.org/x/text v0.43.0 // indirect
)
