package stdlib

import quickjs "github.com/go-quickjs/go-quickjs"

// Fetch is a placeholder until the network module lands.
type Fetch struct{ Loop *Loop }

// Network is a placeholder.
func Network(rt *quickjs.Runtime, cfg *Fetch) error { return nil }
