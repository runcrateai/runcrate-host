package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestApplyVerifiesAndSwaps(t *testing.T) {
	newBin := []byte("#!/bin/sh\necho new\n")
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(newBin) }))
	defer srv.Close()
	dir := t.TempDir()
	target := filepath.Join(dir, "runcrate-host")
	_ = os.WriteFile(target, []byte("old"), 0o755)
	sum := sha256.Sum256(newBin)

	bad := hex.EncodeToString(make([]byte, 32))
	if err := Apply(context.Background(), srv.Client(), srv.URL, bad, target); err == nil {
		t.Fatal("a binary with the wrong sha256 was installed")
	}
	if b, _ := os.ReadFile(target); string(b) != "old" {
		t.Fatal("failed update touched the binary")
	}
	if err := Apply(context.Background(), srv.Client(), "http://example.com/x", hex.EncodeToString(sum[:]), target); err == nil {
		t.Fatal("plain http accepted")
	}
	if err := Apply(context.Background(), srv.Client(), srv.URL, hex.EncodeToString(sum[:]), target); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != string(newBin) {
		t.Fatal("binary not replaced")
	}
	if b, _ := os.ReadFile(target + ".prev"); string(b) != "old" {
		t.Fatal("previous binary not kept")
	}
	if fi, _ := os.Stat(target); fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v", fi.Mode())
	}
}
