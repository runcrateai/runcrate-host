package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaimDataDir(t *testing.T) {
	preMarker := []string{"vms/vm-a/disk.qcow2", "vms/.rec-vm-a.json", "vms/.wipe-vm-b/", "images/20261026-1.qcow2", "holds/0", "state.json", "gpus-cache.json"}
	for _, tc := range []struct {
		name    string
		entries []string // files, or dirs when ending in "/"
		migrate bool
		ok      bool
	}{
		{"new directory", nil, false, true},
		{"fresh filesystem", []string{"lost+found/"}, false, true},
		{"fresh claim of a dir shaped like ours", preMarker, false, false},
		{"partner's files", []string{"lost+found/", "backup.tar"}, false, false},
		{"migrating an install from before the marker", preMarker, true, true},
		// A partner's own KVM storage, /srv/kvm/{images,vms}: never ours.
		{"migrating folders named like ours without agent state", []string{"images/ubuntu.qcow2", "vms/web1/"}, true, false},
		{"migrating images/ with a foreign file", []string{"images/notes.txt", "gpus-cache.json"}, true, false},
		{"migrating vms/ with a foreign file", []string{"vms/disk.img", "gpus-cache.json"}, true, false},
		{"migrating with a foreign top-level file", []string{"gpus-cache.json", "backup.tar"}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "data")
			for _, e := range tc.entries {
				p := filepath.Join(dir, e)
				if strings.HasSuffix(e, "/") {
					if err := os.MkdirAll(p, 0o755); err != nil {
						t.Fatal(err)
					}
					continue
				}
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			err := ClaimDataDir(dir, tc.migrate)
			if (err == nil) != tc.ok {
				t.Fatalf("ClaimDataDir = %v, want ok=%v", err, tc.ok)
			}
			if got := OwnsDataDir(dir); got != tc.ok {
				t.Fatalf("OwnsDataDir = %v after the claim, want %v", got, tc.ok)
			}
			// Once claimed, anything may live there.
			if tc.ok {
				if err := os.WriteFile(filepath.Join(dir, "other"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := ClaimDataDir(dir, false); err != nil {
					t.Fatalf("second claim: %v", err)
				}
			}
		})
	}
}
