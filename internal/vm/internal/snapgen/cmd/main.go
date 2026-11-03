// Command cmd writes internal/vm's zzjit_snapshot_gen.go and
// zzjit_asked_gen_test.go. It is run by go generate in internal/vm.
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
