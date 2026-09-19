// Command cjkgen writes the independently compressed CJK collation overlays.
// Run it from the repository root with:
//
//	go run ./internal/icu/internal/cjkgen
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/klauspost/compress/zstd"
)

type table struct {
	name, locale, script string
}

var tables = []table{
	{"han-pinyin", "zh-u-co-pinyin", "han"},
	{"han-stroke", "zh-u-co-stroke", "han"},
	{"han-zhuyin", "zh-u-co-zhuyin", "han"},
	{"han-unihan", "zh-u-co-unihan", "han"},
	{"han-ja", "ja", "han"},
	{"kana-ja", "ja", "kana"},
	{"han-ko", "ko", "han"},
	{"hangul-ko", "ko", "hangul"},
	{"hangul-searchjl", "ko-u-co-searchjl", "hangul"},
}

func main() {
	root, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	script := filepath.Join(root, "internal", "icu", "internal", "cldrgen", "cjkcollation.mjs")
	if _, err := os.Stat(script); err != nil {
		panic("run cjkgen from the repository root")
	}
	encoder, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(19)),
		zstd.WithEncoderConcurrency(1), zstd.WithEncoderCRC(false))
	if err != nil {
		panic(err)
	}
	defer encoder.Close()

	var out bytes.Buffer
	out.WriteString("QJCK\x01")
	out.WriteByte(byte(len(tables)))
	for _, spec := range tables {
		raw, err := exec.Command("node", script, spec.locale, spec.script).Output()
		if err != nil {
			panic(fmt.Errorf("generating %s: %w", spec.name, err))
		}
		packed := encoder.EncodeAll(raw, nil)
		writeBytes(&out, []byte(spec.name))
		writeBytes(&out, packed)
	}

	path := filepath.Join(root, "internal", "icu", "cjkcollation.bin")
	temporary, err := os.CreateTemp(filepath.Dir(path), ".cjkcollation-*.bin")
	if err != nil {
		panic(err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.Write(out.Bytes()); err != nil {
		temporary.Close()
		panic(err)
	}
	if err := temporary.Close(); err != nil {
		panic(err)
	}
	if err := os.Chmod(temporaryPath, 0o644); err != nil {
		panic(err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		panic(err)
	}
}

func writeBytes(out *bytes.Buffer, value []byte) {
	var size [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(size[:], uint64(len(value)))
	out.Write(size[:n])
	out.Write(value)
}
