package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runcrateai/runcrate-host/internal/config"
)

// A host registered before the data-dir marker existed gets it from
// `register` (what the installer runs again), without a token or a call to
// Runcrate, so `run` starts again.
func TestRegisterAlreadyRegisteredClaimsDataDir(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(filepath.Join(data, "vms"), 0o711); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(data, gpuCacheFile), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg.HostID, cfg.DataDir, cfg.APIURL = "host-1", data, "http://127.0.0.1:1/unreachable"
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if err := cfg.SaveCredential("rch_host_test"); err != nil {
		t.Fatal(err)
	}
	if err := cmdRegister(context.Background(), dir, nil); err != nil {
		t.Fatal(err)
	}
	if !config.OwnsDataDir(data) {
		t.Fatal("register on a registered host left the data directory unclaimed")
	}
}

// The token file must be private to its owner: a group- or world-readable
// one is refused, since another user could have read (or written) it.
func TestReadTokenRefusesSharedFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte("rcp_reg_abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o640); err != nil { // not subject to the umask
		t.Fatal(err)
	}
	if _, err := readToken(p, nil); err == nil {
		t.Fatal("a group-readable token file was accepted")
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	if tok, err := readToken(p, nil); err != nil || tok != "rcp_reg_abc" {
		t.Fatalf("0600 token file: %q, %v", tok, err)
	}
	if tok, err := readToken("-", strings.NewReader("  rcp_reg_stdin \nignored\n")); err != nil || tok != "rcp_reg_stdin" {
		t.Fatalf("stdin token: %q, %v", tok, err)
	}
}
