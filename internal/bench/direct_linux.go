package bench

import (
	"errors"
	"os"
	"syscall"
)

// openDirect bypasses the page cache, so the numbers are the disk's, not
// RAM's. Filesystems without O_DIRECT (tmpfs) fall back to buffered I/O.
func openDirect(p string, flag int) (*os.File, error) {
	f, err := os.OpenFile(p, flag|syscall.O_DIRECT, 0o600)
	if errors.Is(err, syscall.EINVAL) {
		return os.OpenFile(p, flag, 0o600)
	}
	return f, err
}
