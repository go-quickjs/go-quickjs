// Command bundlefetch downloads the pinned open-source compiler test bundles.
//
//	go run ./internal/cmd/bundlefetch -dir /tmp/quickjs-bundles
package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/go-quickjs/go-quickjs/internal/bundletest"
)

func main() {
	dir := flag.String("dir", filepath.Join(os.TempDir(), "quickjs-bundles"), "bundle cache directory")
	flag.Parse()
	client := &http.Client{Timeout: time.Minute}
	for _, bundle := range bundletest.All {
		if err := bundletest.Fetch(client, *dir, bundle); err != nil {
			fmt.Fprintln(os.Stderr, "bundlefetch:", err)
			os.Exit(1)
		}
		fmt.Printf("verified %s (%d bytes)\n", filepath.Join(*dir, bundle.File), bundle.Size)
	}
}
