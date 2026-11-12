package agent

import (
	"encoding/json"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/runcrateai/runcrate-host/internal/api"
	"github.com/runcrateai/runcrate-host/internal/config"
	"github.com/runcrateai/runcrate-host/internal/hw"
)

// PendingUpdate is an update_agent command waiting for the new binary to
// start and report it.
type PendingUpdate struct {
	CommandID int64  `json:"command_id"`
	Version   string `json:"version"`
}

// Outgoing is a command result not yet accepted by the control plane.
type Outgoing struct {
	CommandID int64             `json:"command_id"`
	Result    api.CommandResult `json:"result"`
}

// DestroyedVM is what a destroy_vm remembers about the VM it removed, so a
// retried destroy (the VM's record is gone by then) still says whose GPUs they were.
type DestroyedVM struct {
	Vendor hw.Vendor `json:"vendor"`
	At     time.Time `json:"at"`
}

// State is what the agent must remember across restarts.
type State struct {
	Outbox []Outgoing          `json:"outbox"`
	Done   map[int64]time.Time `json:"done"`
	// Destroyed is the GPU vendor of VMs destroyed on a host with Gaudi
	// modules, by VM id (kept a week, like Done).
	Destroyed     map[string]DestroyedVM `json:"destroyed,omitempty"`
	PendingUpdate *PendingUpdate         `json:"pending_update,omitempty"`

	path string
	mu   sync.Mutex
}

// LoadState reads path (missing = empty).
func LoadState(path string) (*State, error) {
	s := &State{path: path, Done: map[int64]time.Time{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, err
	}
	if s.Done == nil {
		s.Done = map[int64]time.Time{}
	}
	s.path = path
	return s, nil
}

// update mutates under the lock and saves.
func (s *State) update(fn func(s *State)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s)
	// Forget finished commands after a week; the control plane stopped
	// sending them long before.
	for id, at := range s.Done {
		if time.Since(at) > 7*24*time.Hour {
			delete(s.Done, id)
		}
	}
	for id, d := range s.Destroyed {
		if time.Since(d.At) > 7*24*time.Hour {
			delete(s.Destroyed, id)
		}
	}
	if s.path == "" {
		return nil
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return config.WriteFileAtomic(s.path, b, 0o600)
}

func (s *State) view(fn func(s *State)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s)
}
