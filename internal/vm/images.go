package vm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	"github.com/runcrateai/runcrate-host/internal/hw"
	"github.com/runcrateai/runcrate-host/internal/sysexec"
	"github.com/runcrateai/runcrate-host/internal/update"
)

// MaxImageBytes bounds a base image download (the current image is under 2 GB).
const MaxImageBytes = 32 << 30

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
	Run  sysexec.Runner
	// Verify checks the release signature over (version, sha256).
	Verify func(ctx context.Context, img *api.Image) error
	// Used lists versions still under VM disks (kept when pruning).
	Used InUse

	mu      sync.Mutex
	flights map[string]*flight
}

func (s *ImageStore) path(version string) string { return filepath.Join(s.Dir, version+".qcow2") }

// Ensure returns the local path of an image version ("" = current),
// downloading it when missing. Concurrent callers for one version share one
// download; waiting honours ctx.
func (s *ImageStore) Ensure(ctx context.Context, version string) (string, string, error) {
	if version != "" && !versionRe.MatchString(version) {
		return "", "", fmt.Errorf("bad image version %q", version)
	}
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
	if err := s.once(ctx, img); err != nil {
		return "", "", err
	}
	if version == "" {
		s.prune(img.Version)
	}
	return s.path(img.Version), img.Version, nil
}

// Local returns version if present, else the newest image on disk, and
// only asks the API when there is none.
func (s *ImageStore) Local(ctx context.Context, version string) (string, string, error) {
	if version != "" {
		return s.Ensure(ctx, version)
	}
	ents, _ := os.ReadDir(s.Dir)
	var newest os.DirEntry
	var at time.Time
	for _, e := range ents {
		v, ok := strings.CutSuffix(e.Name(), ".qcow2")
		if !ok || !versionRe.MatchString(v) {
			continue
		}
		if fi, err := e.Info(); err == nil && fi.ModTime().After(at) {
			newest, at = e, fi.ModTime()
		}
	}
	if newest != nil {
		v := strings.TrimSuffix(newest.Name(), ".qcow2")
		return s.path(v), v, nil
	}
	return s.Ensure(ctx, "")
}

type flight struct {
	done chan struct{}
	err  error
}

// once runs one download per version at a time.
func (s *ImageStore) once(ctx context.Context, img *api.Image) error {
	s.mu.Lock()
	if s.flights == nil {
		s.flights = map[string]*flight{}
	}
	f, running := s.flights[img.Version]
	if !running {
		f = &flight{done: make(chan struct{})}
		s.flights[img.Version] = f
	}
	s.mu.Unlock()
	if !running {
		// The download outlives one caller's ctx; others may be waiting.
		go func() {
			f.err = s.download(context.WithoutCancel(ctx), img)
			s.mu.Lock()
			delete(s.flights, img.Version)
			s.mu.Unlock()
			close(f.done)
		}()
	}
	select {
	case <-f.done:
		return f.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// InUse reports image versions VM disks are layered on.
type InUse func() map[string]bool

// prune deletes images other than keep that no VM uses.
func (s *ImageStore) prune(keep string) {
	if s.Used == nil {
		return
	}
	used := s.Used()
	ents, _ := os.ReadDir(s.Dir)
	for _, e := range ents {
		v, ok := strings.CutSuffix(e.Name(), ".qcow2")
		if !ok || v == keep || used[v] {
			continue
		}
		_ = os.Remove(filepath.Join(s.Dir, e.Name()))
	}
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
	if s.Verify != nil {
		if err := s.Verify(ctx, img); err != nil {
			return fmt.Errorf("image signature: %w", err)
		}
	}
	// 0711/0444: the unprivileged QEMU reads images, nobody else lists them.
	if err := os.MkdirAll(s.Dir, 0o711); err != nil {
		return err
	}
	_ = os.Chmod(s.Dir, 0o711)
	client := s.HTTP
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Hour, CheckRedirect: update.HTTPSOnly}
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
	// The signature covers the hash, not the size or URL: bound what a
	// download may take from the disk VMs live on.
	if resp.ContentLength > MaxImageBytes {
		return fmt.Errorf("download image: %d bytes is over the %d byte limit", resp.ContentLength, int64(MaxImageBytes))
	}
	if free, _ := hw.DiskFree(s.Dir); free > 0 && resp.ContentLength > 0 && uint64(resp.ContentLength)+uint64(ReserveDiskGB)<<30 > free {
		return fmt.Errorf("download image: %d bytes don't fit (%d free, %d GB kept for the host)", resp.ContentLength, free, ReserveDiskGB)
	}
	tmp, err := os.CreateTemp(s.Dir, ".download-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	// A chunked answer has no length to check up front: keep checking the
	// disk while it writes.
	n, err := io.Copy(io.MultiWriter(&diskGuard{w: tmp, dir: s.Dir, pending: diskGuardEvery}, h), io.LimitReader(resp.Body, MaxImageBytes+1))
	if err == nil && n > MaxImageBytes {
		err = fmt.Errorf("over the %d byte limit", int64(MaxImageBytes))
	}
	if err != nil {
		tmp.Close()
		return fmt.Errorf("download image: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("image sha256 mismatch: got %s, want %s", got, want)
	}
	if err := s.selfContained(ctx, tmp.Name()); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o444); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path(img.Version))
}

// selfContained refuses an image that is not a plain qcow2: a backing file
// or an external data file in its header could point QEMU at host disks.
func (s *ImageStore) selfContained(ctx context.Context, p string) error {
	if s.Run == nil {
		return nil
	}
	out, err := s.Run.Run(ctx, "qemu-img", "info", "-f", "qcow2", "--output=json", p)
	if err != nil {
		return fmt.Errorf("image is not a readable qcow2: %w", err)
	}
	var info struct {
		Format  string `json:"format"`
		Backing string `json:"backing-filename"`
		Spec    struct {
			Data struct {
				DataFile string `json:"data-file"`
			} `json:"data"`
		} `json:"format-specific"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return fmt.Errorf("image info: %w", err)
	}
	if info.Format != "qcow2" || info.Backing != "" || info.Spec.Data.DataFile != "" {
		return fmt.Errorf("image must be a self-contained qcow2 (format %q, backing %q, data file %q)", info.Format, info.Backing, info.Spec.Data.DataFile)
	}
	return nil
}

// diskGuardEvery is how often (in bytes written) diskGuard checks the disk.
const diskGuardEvery = 256 << 20

// diskGuard fails a write once the filesystem holding dir has less than the
// host reserve free.
type diskGuard struct {
	w       io.Writer
	dir     string
	pending int64
}

func (g *diskGuard) Write(p []byte) (int, error) {
	if g.pending += int64(len(p)); g.pending >= diskGuardEvery {
		g.pending = 0
		if free, _ := hw.DiskFree(g.dir); free > 0 && free < uint64(ReserveDiskGB)<<30 {
			return 0, fmt.Errorf("disk nearly full (%d bytes free, %d GB kept for the host)", free, ReserveDiskGB)
		}
	}
	return g.w.Write(p)
}
