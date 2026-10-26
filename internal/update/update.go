// Package update replaces the agent binary with a new release: download over
// HTTPS, check the sha256 the control plane sent, swap atomically, keep the
// previous binary next to it.
package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// HTTPSOnly is a CheckRedirect that refuses redirects off https.
func HTTPSOnly(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return fmt.Errorf("redirect to %s refused", req.URL.Scheme)
	}
	if len(via) >= 10 {
		return fmt.Errorf("too many redirects")
	}
	return nil
}

// Apply downloads the binary at rawURL, verifies it against sha (hex) and
// replaces target. The caller restarts the process afterwards.
func Apply(ctx context.Context, client *http.Client, rawURL, sha, target string) error {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" {
		return fmt.Errorf("update url must be https")
	}
	want := strings.ToLower(strings.TrimSpace(sha))
	if len(want) != 64 {
		return fmt.Errorf("update sha256 is not a sha256")
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Minute, CheckRedirect: HTTPSOnly}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download update: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download update: HTTP %d", resp.StatusCode)
	}
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".runcrate-host-update-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, 256<<20)); err != nil {
		tmp.Close()
		return fmt.Errorf("download update: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("update sha256 mismatch: got %s, want %s", got, want)
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	// Keep the running binary as .prev for a manual rollback.
	_ = os.Remove(target + ".prev.tmp")
	if err := os.Link(target, target+".prev.tmp"); err == nil {
		_ = os.Rename(target+".prev.tmp", target+".prev")
	}
	return os.Rename(tmp.Name(), target)
}
