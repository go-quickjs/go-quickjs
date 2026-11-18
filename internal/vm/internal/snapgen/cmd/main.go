// Command cmd writes internal/vm's jit_snapshot_gen.go. It is run by go
// generate in internal/vm (generate.go).
package main

import (
	"log"

	"github.com/go-quickjs/go-quickjs/internal/vm/internal/snapgen"
)

func main() {
	if err := snapgen.Write("."); err != nil {
		log.Fatal(err)
	}
}
