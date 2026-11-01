package bundletest

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

// Fetch downloads a pinned bundle into dir, reusing a verified cached copy.
// A failed download leaves any previous copy intact.
func Fetch(client *http.Client, dir string, bundle Bundle) error {
	path := filepath.Join(dir, bundle.File)
	if b, err := os.ReadFile(path); err == nil && int64(len(b)) == bundle.Size &&
		fmt.Sprintf("%x", sha256.Sum256(b)) == bundle.SHA256 {
		return nil
	}
	resp, err := client.Get(bundle.URL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", bundle.URL, resp.Status)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".bundle-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, hash), io.LimitReader(resp.Body, bundle.Size+1))
	if err != nil {
		return err
	}
	if n != bundle.Size {
		return fmt.Errorf("%s: size %d, want %d", bundle.File, n, bundle.Size)
	}
	if got := fmt.Sprintf("%x", hash.Sum(nil)); got != bundle.SHA256 {
		return fmt.Errorf("%s: SHA-256 %s, want %s", bundle.File, got, bundle.SHA256)
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
