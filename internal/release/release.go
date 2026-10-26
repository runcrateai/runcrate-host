// Package release checks Runcrate's release signatures. Agent binaries and
// VM images are signed offline with an ed25519 key whose public half is built
// into the agent, so a compromised control plane can't push code or images:
// it can only point at releases Runcrate signed. The signature covers the
// kind, the version and the sha256, so a signed artifact can't be passed off
// as another version.
package release

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// PublicKey is Runcrate's release signing key (ed25519, base64).
const PublicKey = "YBrMXteHsZh26jJMcMZ3PI05QmTbNcd7gy9ALNlW2Yc="

// Kinds of signed artifacts.
const (
	KindAgent = "runcrate-host"
	KindImage = "runcrate-vm"
)

// Message is what gets signed.
func Message(kind, version, sha256 string) []byte {
	return []byte(kind + "\n" + version + "\n" + strings.ToLower(sha256) + "\n")
}

// Verify checks a base64 signature.
func Verify(kind, version, sha256, sigB64 string) error {
	pub, err := base64.StdEncoding.DecodeString(PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return errors.New("bad built-in release key")
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(sigB64))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return errors.New("signature is not a base64 ed25519 signature")
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), Message(kind, version, sha256), sig) {
		return fmt.Errorf("%s %s is not signed by Runcrate", kind, version)
	}
	return nil
}

// SigURL is where an artifact's signature lives: the same URL with ".sig"
// added to the path.
func SigURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" {
		return "", errors.New("artifact url must be https")
	}
	u.Path += ".sig"
	u.RawPath = ""
	return u.String(), nil
}

// Fetch downloads the signature next to an artifact and verifies it.
func Fetch(ctx context.Context, client *http.Client, kind, version, sha256, artifactURL string) error {
	su, err := SigURL(artifactURL)
	if err != nil {
		return err
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, su, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("signature: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("signature: HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return err
	}
	return Verify(kind, version, sha256, string(b))
}

// Newer reports whether version a is newer than b (semver-ish: optional v,
// numeric major.minor.patch, anything after '-' or '+' ignored). A "dev"
// build accepts anything.
func Newer(a, b string) (bool, error) {
	if b == "dev" {
		return true, nil
	}
	pa, err := parse(a)
	if err != nil {
		return false, err
	}
	pb, err := parse(b)
	if err != nil {
		return false, err
	}
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] > pb[i], nil
		}
	}
	return false, nil
}

func parse(v string) ([3]int, error) {
	var out [3]int
	core := strings.TrimPrefix(v, "v")
	if i := strings.IndexAny(core, "-+"); i >= 0 {
		core = core[:i]
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return out, fmt.Errorf("version %q is not major.minor.patch", v)
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, fmt.Errorf("version %q is not major.minor.patch", v)
		}
		out[i] = n
	}
	return out, nil
}
