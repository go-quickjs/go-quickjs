module github.com/go-quickjs/go-quickjs/internal/jit/verify

go 1.24.0

// The emitters under test are go-quickjs's own, from the checkout this
// module is in. The disassemblers are a dependency of this module only.
replace github.com/go-quickjs/go-quickjs => ../../..

require (
	github.com/go-quickjs/go-quickjs v0.0.0-00010101000000-000000000000
	golang.org/x/arch v0.22.0
)

require golang.org/x/sys v0.41.0 // indirect
