// Package config holds the agent's configuration and its host credential.
// Both live under /etc/runcrate-host, root-only.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/runcrateai/runcrate-host/internal/api"
	"github.com/runcrateai/runcrate-host/internal/hw"
)

// Default paths.
const (
	DefaultDir     = "/etc/runcrate-host"
	DefaultDataDir = "/var/lib/runcrate-host"
	DefaultSubnet  = "10.254.0.0/16"
)

// Config is /etc/runcrate-host/config.json.
type Config struct {
	APIURL string `json:"api_url"`
	HostID string `json:"host_id,omitempty"`
	Name   string `json:"name"`
	Site   string `json:"site,omitempty"`
	// GPUs restricts Runcrate to these PCI addresses; empty means every
	// NVIDIA GPU on the host.
	GPUs    []string `json:"gpus,omitempty"`
	DataDir string   `json:"data_dir"`
	// VMSubnet is carved into one /30 per VM (host .1, guest .2).
	VMSubnet string `json:"vm_subnet"`
	// ForwardWasOff records that IPv4 forwarding was off before install, so
	// the agent drops forwarding that doesn't involve its VMs and uninstall
	// turns it back off.
	ForwardWasOff bool `json:"forward_was_off"`
	// ScrubGPUs boots a throwaway VM that overwrites GPU memory after every
	// customer VM, so the next customer can't read the last one's data.
	ScrubGPUs bool `json:"scrub_gpus"`

	dir string
}

// Dir is where the config lives.
func (c *Config) Dir() string { return c.dir }

// Path of config.json.
func (c *Config) Path() string { return filepath.Join(c.dir, "config.json") }

// CredentialPath is the host credential file (0600 root).
func (c *Config) CredentialPath() string { return filepath.Join(c.dir, "credential") }

// Defaults fills empty fields.
func (c *Config) Defaults() {
	if c.APIURL == "" {
		c.APIURL = api.DefaultBaseURL
	}
	if c.DataDir == "" {
		c.DataDir = DefaultDataDir
	}
	if c.VMSubnet == "" {
		c.VMSubnet = DefaultSubnet
	}
	if c.Name == "" {
		c.Name, _ = os.Hostname()
	}
}

// Validate checks the fields an operator can get wrong.
func (c *Config) Validate() error {
	_, n, err := net.ParseCIDR(c.VMSubnet)
	if err != nil || n.IP.To4() == nil {
		return fmt.Errorf("vm_subnet must be an IPv4 CIDR: %q", c.VMSubnet)
	}
	if ones, _ := n.Mask.Size(); ones > 24 {
		return fmt.Errorf("vm_subnet must be /24 or larger: %q", c.VMSubnet)
	}
	for _, g := range c.GPUs {
		if !hw.ValidPCIAddress(g) {
			return fmt.Errorf("gpus: %q is not a PCI address like 0000:41:00.0", g)
		}
	}
	if !filepath.IsAbs(c.DataDir) {
		return fmt.Errorf("data_dir must be absolute: %q", c.DataDir)
	}
	return nil
}

// Load reads dir/config.json (missing file = defaults).
func Load(dir string) (*Config, error) {
	c := &Config{dir: dir, ScrubGPUs: true}
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(b, c); err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Join(dir, "config.json"), err)
		}
	}
	c.dir = dir
	c.Defaults()
	return c, c.Validate()
}

// Save writes config.json atomically, root-only.
func (c *Config) Save() error {
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(c.Path(), append(b, '\n'), 0o600)
}

// Credential reads the host credential ("" when not registered).
func (c *Config) Credential() (string, error) {
	b, err := os.ReadFile(c.CredentialPath())
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if fi, err := os.Stat(c.CredentialPath()); err == nil && fi.Mode().Perm()&0o077 != 0 {
		// Someone loosened it; put it back before using it.
		if err := os.Chmod(c.CredentialPath(), 0o600); err != nil {
			return "", fmt.Errorf("credential file is readable by others and can't be fixed: %w", err)
		}
	}
	return strings.TrimSpace(string(b)), nil
}

// SaveCredential stores the host credential, root-only.
func (c *Config) SaveCredential(cred string) error {
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return err
	}
	return WriteFileAtomic(c.CredentialPath(), []byte(cred+"\n"), 0o600)
}

// WriteFileAtomic writes via a temp file and rename, with mode set before
// any content lands.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
