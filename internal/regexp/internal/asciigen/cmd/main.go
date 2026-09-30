// Command cmd writes internal/regexp's exec_ascii.go and exec_utf8.go from
// exec.go. It is run by go generate in internal/regexp.
package main

import (
	"log"
	"os"

	"github.com/go-quickjs/go-quickjs/internal/regexp/internal/asciigen"
)

func main() {
	for file, v := range map[string]asciigen.Variant{
		"exec_ascii.go": asciigen.ASCII,
		"exec_utf8.go":  asciigen.UTF8,
	} {
		out, err := asciigen.Generate("exec.go", v)
		if err != nil {
			log.Fatal(err)
		}
		if err := os.WriteFile(file, out, 0o644); err != nil {
			log.Fatal(err)
		}
	}
}
