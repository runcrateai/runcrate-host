package vm

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/runcrateai/runcrate-host/internal/sysexec"
)

// UserData is the customer's launch script, verbatim: a shell script or
// cloud-config. Everything of ours (reclaim watcher, datasource, driver)
// is in the image, because the launch script disables cloud-init after its
// first run.
func UserData(customer string) ([]byte, error) {
	c := strings.TrimSpace(customer)
	if c == "" {
		return []byte("#cloud-config\n{}\n"), nil
	}
	if !strings.HasPrefix(c, "#!") && !strings.HasPrefix(c, "#cloud-config") {
		return nil, fmt.Errorf("cloud_init must be a #! script or #cloud-config")
	}
	return []byte(customer), nil
}

// MetaData is the NoCloud meta-data.
func MetaData(vmID, hostname string) string {
	return fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", vmID, hostname)
}

// NetworkConfig is the NoCloud network-config (v2): a static address on the
// VM's /30, public resolvers (the VM can't reach the partner's).
func NetworkConfig(mac, guest, gateway string) string {
	return fmt.Sprintf(`version: 2
ethernets:
  eth0:
    match:
      macaddress: "%s"
    set-name: eth0
    addresses: [%s/30]
    routes:
      - to: default
        via: %s
    nameservers:
      addresses: [1.1.1.1, 8.8.8.8, 9.9.9.9]
`, mac, guest, gateway)
}

// WriteSeed writes the NoCloud files and packs them into an ISO labelled
// cidata.
func WriteSeed(ctx context.Context, run sysexec.Runner, dir string, userData []byte, metaData, netConfig string) (string, error) {
	src := filepath.Join(dir, "seed")
	if err := os.MkdirAll(src, 0o700); err != nil {
		return "", err
	}
	files := map[string][]byte{
		"user-data":      userData,
		"meta-data":      []byte(metaData),
		"network-config": []byte(netConfig),
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(src, name), b, 0o600); err != nil {
			return "", err
		}
	}
	iso := filepath.Join(dir, "seed.iso")
	_, err := run.Run(ctx, "genisoimage", "-quiet", "-output", iso, "-volid", "cidata", "-joliet", "-rock",
		filepath.Join(src, "user-data"), filepath.Join(src, "meta-data"), filepath.Join(src, "network-config"))
	if err != nil {
		return "", err
	}
	// The seed holds the customer's launch script (with its machine token):
	// overwrite the plain copies, keep only the ISO.
	for name := range files {
		p := filepath.Join(src, name)
		if fi, err := os.Stat(p); err == nil {
			_ = os.WriteFile(p, make([]byte, fi.Size()), 0o600)
		}
	}
	_ = os.RemoveAll(src)
	return iso, os.Chmod(iso, 0o600)
}
