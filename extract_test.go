package toolchainext

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestExtractZipFlattens builds a small in-memory zip whose root is "go/"
// (one dir "go/bin/" + a dummy "go/bin/go.exe") and asserts extract flattens
// the wrapping "go/" so the layout lands at <tmp>/bin/go.exe — i.e. when
// extracting into <runtimeDir>/go, the binary ends up at <runtimeDir>/go/bin/go.exe,
// matching tools.go.
func TestExtractZipFlattens(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	// directory entry
	if _, err := zw.Create("go/bin/"); err != nil {
		t.Fatalf("create dir entry: %v", err)
	}
	// file entry
	fw, err := zw.Create("go/bin/go.exe")
	if err != nil {
		t.Fatalf("create file entry: %v", err)
	}
	if _, err := fw.Write([]byte("dummy")); err != nil {
		t.Fatalf("write file entry: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}

	// write the zip to a temp file (extract reads from a path).
	zipPath := filepath.Join(t.TempDir(), "go.zip")
	if err := os.WriteFile(zipPath, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write zip: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "go")
	if err := extract(zipPath, dest, "go"); err != nil {
		t.Fatalf("extract: %v", err)
	}

	got := filepath.Join(dest, "bin", "go.exe")
	fi, err := os.Stat(got)
	if err != nil {
		t.Fatalf("expected flattened binary at %s: %v", got, err)
	}
	if fi.IsDir() {
		t.Fatalf("expected file at %s, got dir", got)
	}
	data, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("read extracted binary: %v", err)
	}
	if string(data) != "dummy" {
		t.Fatalf("content mismatch: got %q want %q", data, "dummy")
	}
}
