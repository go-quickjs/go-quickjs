package bundletest

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestFetch(t *testing.T) {
	const content = "var bundle = 1;"
	for _, response := range []string{"valid", "hash mismatch", "too long", "short", "HTTP error"} {
		t.Run(response, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				switch response {
				case "valid":
					fmt.Fprint(w, content)
				case "hash mismatch":
					fmt.Fprint(w, "var bundle = 2;")
				case "too long":
					fmt.Fprint(w, content+"extra")
				case "short":
					fmt.Fprint(w, "var")
				case "HTTP error":
					http.Error(w, "missing", http.StatusNotFound)
				}
			}))
			defer server.Close()
			bundle := Bundle{File: "bundle.js", URL: server.URL, Size: int64(len(content)), SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(content)))}
			dir := t.TempDir()
			path := filepath.Join(dir, bundle.File)
			if err := os.WriteFile(path, []byte("previous"), 0o600); err != nil {
				t.Fatal(err)
			}
			err := Fetch(server.Client(), dir, bundle)
			want := "previous"
			if response == "valid" {
				if err != nil {
					t.Fatal(err)
				}
				want = content
				// A cached bundle works even when the server is offline.
				server.Close()
				if err := Fetch(server.Client(), dir, bundle); err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("accepted a failed or corrupted download")
			}
			b, err := os.ReadFile(path)
			if err != nil || string(b) != want {
				t.Fatalf("cached file: %q, %v; want %q", b, err, want)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 || requests.Load() != 1 {
				t.Fatalf("entries=%v, requests=%d, error=%v", entries, requests.Load(), err)
			}
		})
	}
}
