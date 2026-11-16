// Command cmd writes internal/vm's native handlers' files: each
// architecture's handlers and plain table (zzzyn_native_handlers_amd64.s)
// and each other variant's table and handlers
// (zzzyn_native_feedback_amd64.s). It is run by go generate in
// internal/vm, whose layout it reads by building and running the helper
// nativegen/cmd/layout. Each file is written to a temporary file and
// moved into place, so that a failed run leaves the committed one.
package main

import (
	"log"
	"os"
	"path/filepath"

	"github.com/go-quickjs/go-quickjs/internal/vm/internal/nativegen"
)

func main() {
	files, err := nativegen.Generate()
	if err != nil {
		log.Fatal(err)
	}
	for _, f := range files {
		tmp, err := os.CreateTemp(".", "."+filepath.Base(f.Name)+".*.tmp")
		if err != nil {
			log.Fatal(err)
		}
		_, err = tmp.Write(f.Data)
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Chmod(tmp.Name(), 0o644)
		}
		if err == nil {
			err = os.Rename(tmp.Name(), f.Name)
		}
		if err != nil {
			os.Remove(tmp.Name())
			log.Fatal(err)
		}
	}
}
