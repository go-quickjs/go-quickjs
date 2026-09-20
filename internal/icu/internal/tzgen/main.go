// Command tzgen rewrites Go's full-history zone archive to the slim-link
// layout used by ICU. Run it after copying zoneinfo.zip from the matching Go
// tzdata release.
package main

import (
	"archive/zip"
	_ "embed"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"sort"
	"strings"
)

//go:embed slim-links.txt
var slimLinks string

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: go run ./internal/icu/internal/tzgen zoneinfo.zip")
		os.Exit(2)
	}
	name := os.Args[1]
	reader, err := zip.OpenReader(name)
	if err != nil {
		panic(err)
	}
	files := make(map[string][]byte, len(reader.File))
	for _, file := range reader.File {
		opened, err := file.Open()
		if err != nil {
			panic(err)
		}
		data, err := io.ReadAll(opened)
		if err != nil {
			panic(err)
		}
		if err := opened.Close(); err != nil {
			panic(err)
		}
		files[file.Name] = data
	}
	if err := reader.Close(); err != nil {
		panic(err)
	}

	for _, line := range strings.Split(slimLinks, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		target, alias := fields[0], fields[1]
		data, targetOK := files[target]
		_, aliasOK := files[alias]
		if targetOK && aliasOK {
			files[alias] = data
		}
	}

	temporary := name + ".tmp"
	output, err := os.Create(temporary)
	if err != nil {
		panic(err)
	}
	writer := zip.NewWriter(output)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		data := files[name]
		entry, err := writer.CreateRaw(&zip.FileHeader{
			Name:               name,
			Method:             zip.Store,
			CompressedSize64:   uint64(len(data)),
			UncompressedSize64: uint64(len(data)),
			CRC32:              crc32.ChecksumIEEE(data),
		})
		if err != nil {
			panic(err)
		}
		if _, err := entry.Write(data); err != nil {
			panic(err)
		}
	}
	if err := writer.Close(); err != nil {
		panic(err)
	}
	if err := output.Close(); err != nil {
		panic(err)
	}
	if err := os.Rename(temporary, name); err != nil {
		panic(err)
	}
}
