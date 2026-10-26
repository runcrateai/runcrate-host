//go:build !linux

package bench

import "os"

func openDirect(p string, flag int) (*os.File, error) { return os.OpenFile(p, flag, 0o600) }
