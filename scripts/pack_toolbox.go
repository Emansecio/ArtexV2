//go:build ignore

package main

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/klauspost/compress/zstd"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintf(os.Stderr, "usage: go run ./scripts/pack_toolbox.go <src-bin-dir> <out.tar.zst>\n")
		os.Exit(2)
	}
	srcDir, outPath := os.Args[1], os.Args[2]
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read dir: %v\n", err)
		os.Exit(1)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	if len(files) == 0 {
		fmt.Fprintf(os.Stderr, "no binaries found in %s\n", srcDir)
		os.Exit(1)
	}

	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "mkdir out: %v\n", err)
		os.Exit(1)
	}
	tmpPath := outPath + ".tmp"
	out, err := os.Create(tmpPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create out: %v\n", err)
		os.Exit(1)
	}
	hasher := sha256.New()
	zw, err := zstd.NewWriter(io.MultiWriter(out, hasher),
		zstd.WithEncoderLevel(zstd.SpeedBestCompression),
		zstd.WithWindowSize(32<<20),
	)
	if err != nil {
		_ = out.Close()
		fmt.Fprintf(os.Stderr, "zstd writer: %v\n", err)
		os.Exit(1)
	}
	tw := tar.NewWriter(zw)

	for _, name := range files {
		full := filepath.Join(srcDir, name)
		fi, err := os.Stat(full)
		if err != nil {
			fmt.Fprintf(os.Stderr, "stat %s: %v\n", name, err)
			os.Exit(1)
		}
		if err := tw.WriteHeader(&tar.Header{
			Name:     name,
			Mode:     0o755,
			Size:     fi.Size(),
			Typeflag: tar.TypeReg,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "tar header %s: %v\n", name, err)
			os.Exit(1)
		}
		f, err := os.Open(full)
		if err != nil {
			fmt.Fprintf(os.Stderr, "open %s: %v\n", name, err)
			os.Exit(1)
		}
		if _, err := io.Copy(tw, f); err != nil {
			_ = f.Close()
			fmt.Fprintf(os.Stderr, "copy %s: %v\n", name, err)
			os.Exit(1)
		}
		_ = f.Close()
	}

	if err := tw.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "close tar: %v\n", err)
		os.Exit(1)
	}
	if err := zw.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "close zstd: %v\n", err)
		os.Exit(1)
	}
	if err := out.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "close file: %v\n", err)
		os.Exit(1)
	}
	if err := os.Rename(tmpPath, outPath); err != nil {
		fmt.Fprintf(os.Stderr, "rename out: %v\n", err)
		os.Exit(1)
	}
	hashPath := strings.TrimSuffix(outPath, ".tar.zst") + ".sha256"
	sumHex := hex.EncodeToString(hasher.Sum(nil)) + "\n"
	if err := os.WriteFile(hashPath, []byte(sumHex), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "write sha256: %v\n", err)
		os.Exit(1)
	}
}
