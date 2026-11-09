module github.com/go-quickjs/go-quickjs/internal/cmd/wasmbench/wazero

go 1.24.0

// The worker package is go-quickjs's own, from the checkout this module is
// in.
replace github.com/go-quickjs/go-quickjs => ../../../..

require (
	github.com/go-quickjs/go-quickjs v0.0.0-00010101000000-000000000000
	github.com/tetratelabs/wazero v1.9.0
)
