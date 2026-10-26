// Package sysexec runs host commands behind an interface tests can fake.
package sysexec

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Runner runs a command and returns its combined output.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
	// RunInput is Run with stdin.
	RunInput(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error)
}

// Exec runs real commands.
type Exec struct{}

// Run implements Runner.
func (Exec) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return Exec{}.RunInput(ctx, nil, name, args...)
}

// RunInput implements Runner.
func (Exec) RunInput(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 800 {
			msg = msg[len(msg)-800:]
		}
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
	}
	return out, nil
}
