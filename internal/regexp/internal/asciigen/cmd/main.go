// Command cmd writes internal/regexp/exec_ascii.go from exec.go. It is run by
// go generate in internal/regexp.
package main

import (
	"log"
	"os"

	"github.com/go-quickjs/go-quickjs/internal/regexp/internal/asciigen"
)

func main() {
	out, err := asciigen.Generate("exec.go")
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile("exec_ascii.go", out, 0o644); err != nil {
		log.Fatal(err)
	}
}
