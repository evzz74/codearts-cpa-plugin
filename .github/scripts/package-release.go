package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// package-release 把一个编译好的 CPA 插件库打成交付 zip：zip 根目录直接是
// codearts.so / .dylib / .dll，符合 CLIProxyAPI pluginstore 的 ArchiveName 预期。
func main() {
	libraryPath := flag.String("library", "", "compiled plugin library")
	archivePath := flag.String("archive", "", "output zip archive")
	checksumPath := flag.String("checksum", "", "output checksum file")
	flag.Parse()

	if *libraryPath == "" || *archivePath == "" || *checksumPath == "" {
		fatalf("library, archive, and checksum are required")
	}
	archiveData, err := packageLibrary(*libraryPath, *archivePath)
	if err != nil {
		fatalf("%v", err)
	}
	sum := sha256.Sum256(archiveData)
	line := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), filepath.Base(*archivePath))
	if err := os.WriteFile(*checksumPath, []byte(line), 0o644); err != nil {
		fatalf("write checksum: %v", err)
	}
}

func packageLibrary(libraryPath, archivePath string) ([]byte, error) {
	library, err := os.Open(libraryPath)
	if err != nil {
		return nil, fmt.Errorf("open library: %w", err)
	}
	defer library.Close()

	info, err := library.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat library: %w", err)
	}
	archive, err := os.Create(archivePath)
	if err != nil {
		return nil, fmt.Errorf("create archive: %w", err)
	}
	defer archive.Close()

	writer := zip.NewWriter(archive)
	header, err := zip.FileInfoHeader(info)
	if err != nil {
		return nil, fmt.Errorf("create zip header: %w", err)
	}
	header.Name = filepath.Base(libraryPath)
	header.Method = zip.Deflate
	header.SetMode(0o755)
	entry, err := writer.CreateHeader(header)
	if err != nil {
		return nil, fmt.Errorf("create zip entry: %w", err)
	}
	if _, err := io.Copy(entry, library); err != nil {
		return nil, fmt.Errorf("copy library: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("close zip writer: %w", err)
	}
	if err := archive.Close(); err != nil {
		return nil, fmt.Errorf("close archive: %w", err)
	}
	return os.ReadFile(archivePath)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
