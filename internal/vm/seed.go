package vm

import (
	"bytes"
	"context"
	"fmt"
	"mime/multipart"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"

	"github.com/runcrateai/runcrate-host/internal/sysexec"
)

// reclaimWatch runs inside every VM: it copies the host's reclaim notice to
// /run/runcrate/reclaim-notice.json, so jobs can watch a file as well as the
// metadata endpoint.
const reclaimWatch = `#!/bin/sh
# Runcrate: mirror the reclaim notice from the metadata server into a file.
d=/run/runcrate
mkdir -p "$d"
while :; do
  if curl -fsS --max-time 3 -o "$d/.reclaim-notice.part" http://169.254.169.254/runcrate/v1/reclaim-notice 2>/dev/null; then
    mv -f "$d/.reclaim-notice.part" "$d/reclaim-notice.json"
  else
    rm -f "$d/.reclaim-notice.part"
  fi
  sleep 5
done
`

const reclaimUnit = `[Unit]
Description=Runcrate reclaim notice watcher
After=network-online.target
Wants=network-online.target
[Service]
ExecStart=/usr/local/bin/runcrate-reclaim-watch
Restart=always
RestartSec=5
[Install]
WantedBy=multi-user.target
`

// hostConfig is our own cloud-config part. It only writes files and enables
// a unit in bootcmd: the customer's launch script disables cloud-init and
// masks cloud-final once it runs, so nothing of ours may depend on the final
// stage.
func hostConfig() string {
	indent := func(s string) string {
		return "      " + strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n      ") + "\n"
	}
	return "#cloud-config\n" +
		"datasource_list: [NoCloud, None]\n" +
		"write_files:\n" +
		"  - path: /usr/local/bin/runcrate-reclaim-watch\n" +
		"    permissions: '0755'\n" +
		"    content: |\n" + indent(reclaimWatch) +
		"  - path: /etc/systemd/system/runcrate-reclaim-watch.service\n" +
		"    permissions: '0644'\n" +
		"    content: |\n" + indent(reclaimUnit) +
		"bootcmd:\n" +
		"  - [systemctl, daemon-reload]\n" +
		"  - [systemctl, enable, --now, --no-block, runcrate-reclaim-watch.service]\n"
}

// UserData builds the multipart user-data: our cloud-config, then the
// customer's launch script verbatim (a shell script, or cloud-config if the
// control plane ever sends one).
func UserData(customer string) ([]byte, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fmt.Fprintf(&buf, "Content-Type: multipart/mixed; boundary=\"%s\"\nMIME-Version: 1.0\n\n", mw.Boundary())
	add := func(ctype, name, body string) error {
		h := textproto.MIMEHeader{}
		h.Set("Content-Type", ctype+`; charset="utf-8"`)
		h.Set("MIME-Version", "1.0")
		h.Set("Content-Transfer-Encoding", "8bit")
		h.Set("Content-Disposition", `attachment; filename="`+name+`"`)
		w, err := mw.CreatePart(h)
		if err != nil {
			return err
		}
		_, err = w.Write([]byte(body))
		return err
	}
	if err := add("text/cloud-config", "runcrate-host.yaml", hostConfig()); err != nil {
		return nil, err
	}
	if c := strings.TrimSpace(customer); c != "" {
		ctype := "text/x-shellscript"
		if strings.HasPrefix(c, "#cloud-config") {
			ctype = "text/cloud-config"
		} else if !strings.HasPrefix(c, "#!") {
			return nil, fmt.Errorf("cloud_init must be a #! script or #cloud-config")
		}
		if err := add(ctype, "launch", customer); err != nil {
			return nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
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
	// The seed holds the customer's launch script (with its machine token);
	// keep only the ISO, root-only.
	_ = os.RemoveAll(src)
	return iso, os.Chmod(iso, 0o600)
}
