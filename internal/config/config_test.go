package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestClaimDataDir(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []string // files, or dirs when ending in "/"
		ok      bool
	}{
		{"new directory", nil, true},
		{"fresh filesystem", []string{"lost+found/"}, true},
		{"install from before the marker", []string{"vms/", "images/", "holds/", "state.json", "gpus-cache.json"}, true},
		{"partner's files", []string{"lost+found/", "backup.tar"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "data")
			for _, e := range tc.entries {
				p := filepath.Join(dir, e)
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if e[len(e)-1] == '/' {
					if err := os.Mkdir(p, 0o755); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(p, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			err := ClaimDataDir(dir)
			if (err == nil) != tc.ok {
				t.Fatalf("ClaimDataDir = %v, want ok=%v", err, tc.ok)
			}
			if OwnsDataDir(dir) != tc.ok {
				t.Fatalf("OwnsDataDir = %v after the claim, want %v", !tc.ok, tc.ok)
			}
			// Once claimed, anything may live there.
			if tc.ok {
				if err := os.WriteFile(filepath.Join(dir, "other"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := ClaimDataDir(dir); err != nil {
					t.Fatalf("second claim: %v", err)
				}
			}
		})
	}
}
