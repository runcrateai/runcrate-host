package vm

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
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/runcrateai/runcrate-host/internal/api"
)

var versionRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ImageAPI is the part of the host API the image store needs.
type ImageAPI interface {
	Image(ctx context.Context, version string) (*api.Image, error)
}

// ImageStore downloads base images once, checks their sha256 and keeps them
// read-only under dataDir/images.
type ImageStore struct {
	Dir  string
	API  ImageAPI
	HTTP *http.Client

	mu sync.Mutex
}

func (s *ImageStore) path(version string) string { return filepath.Join(s.Dir, version+".qcow2") }

// Ensure returns the local path of an image version ("" = current),
// downloading it when missing.
func (s *ImageStore) Ensure(ctx context.Context, version string) (string, string, error) {
	if version != "" && !versionRe.MatchString(version) {
		return "", "", fmt.Errorf("bad image version %q", version)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if version != "" {
		if _, err := os.Stat(s.path(version)); err == nil {
			return s.path(version), version, nil
		}
	}
	img, err := s.API.Image(ctx, version)
	if err != nil {
		return "", "", err
	}
	if !versionRe.MatchString(img.Version) || (version != "" && img.Version != version) {
		return "", "", fmt.Errorf("image api answered version %q for %q", img.Version, version)
	}
	if _, err := os.Stat(s.path(img.Version)); err == nil {
		return s.path(img.Version), img.Version, nil
	}
	if err := s.download(ctx, img); err != nil {
		return "", "", err
	}
	return s.path(img.Version), img.Version, nil
}

func (s *ImageStore) download(ctx context.Context, img *api.Image) error {
	u, err := url.Parse(img.URL)
	if err != nil || u.Scheme != "https" {
		return fmt.Errorf("image url must be https: %q", img.URL)
	}
	want := strings.ToLower(strings.TrimSpace(img.SHA256))
	if len(want) != 64 {
		return fmt.Errorf("image sha256 %q is not a sha256", img.SHA256)
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	client := s.HTTP
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Hour}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, img.URL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download image: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download image: HTTP %d", resp.StatusCode)
	}
	tmp, err := os.CreateTemp(s.Dir, ".download-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		tmp.Close()
		return fmt.Errorf("download image: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("image sha256 mismatch: got %s, want %s", got, want)
	}
	if err := os.Chmod(tmp.Name(), 0o400); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path(img.Version))
}

// Prune deletes images other than keep that no VM disk uses.
func (s *ImageStore) Prune(keep string, inUse map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ents, _ := os.ReadDir(s.Dir)
	for _, e := range ents {
		v, ok := strings.CutSuffix(e.Name(), ".qcow2")
		if !ok || v == keep || inUse[v] {
			continue
		}
		_ = os.Remove(filepath.Join(s.Dir, e.Name()))
	}
}
