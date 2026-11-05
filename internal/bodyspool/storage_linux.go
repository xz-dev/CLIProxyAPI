//go:build linux

package bodyspool

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

const (
	tmpfsMagic = 0x01021994
	ramfsMagic = 0x858458f6
)

// openTmpFile opens an anonymous 0600 spool file; its blocks are freed when the last fd closes.
// There is no named-file fallback (D3).
func openTmpFile(dir string) (*os.File, error) {
	fd, err := unix.Open(dir, unix.O_TMPFILE|unix.O_RDWR|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("bodyspool: open O_TMPFILE spool in %s: %w", dir, err)
	}
	return os.NewFile(uintptr(fd), dir+"/(anonymous spool)"), nil
}

// syncDrop makes a written range durable and drops it from the page cache.
func syncDrop(f *os.File, off, n int64) error {
	if err := unix.Fdatasync(int(f.Fd())); err != nil {
		return fmt.Errorf("bodyspool: fdatasync spool: %w", err)
	}
	return dropRange(f, off, n)
}

// dropRange drops a clean range from the page cache (reader side).
func dropRange(f *os.File, off, n int64) error {
	if err := unix.Fadvise(int(f.Fd()), off, n, unix.FADV_DONTNEED); err != nil {
		return fmt.Errorf("bodyspool: fadvise spool: %w", err)
	}
	return nil
}

func freeBytes(dir string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Frsize), nil
}

// checkFloor refuses a write of next bytes that would leave less than reserve bytes free.
func checkFloor(dir string, next, reserve int64, hop string) error {
	free, err := freeBytes(dir)
	if err != nil {
		return fmt.Errorf("bodyspool: statfs %s: %w", dir, err)
	}
	if free-next < reserve {
		return &Error{Status: 507, Code: "insufficient_storage", Hop: hop, Limit: reserve,
			Message: fmt.Sprintf("%s: spool write of %d bytes would leave %d bytes free, below the %d byte disk reserve", hop, next, free-next, reserve)}
	}
	return nil
}

// CheckDir validates a spool directory at startup (D3): on disk (not tmpfs or ramfs), mode 0700,
// O_TMPFILE supported, and holding no regular file. Every failure is an explicit configuration error.
func CheckDir(dir string) error {
	if dir == "" {
		return fmt.Errorf("bodyspool: spool directory is not configured")
	}
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return fmt.Errorf("bodyspool: statfs spool directory %s: %w", dir, err)
	}
	if uint32(st.Type) == tmpfsMagic || uint32(st.Type) == ramfsMagic {
		return fmt.Errorf("bodyspool: spool directory %s is on a memory filesystem (tmpfs/ramfs); a disk volume is required", dir)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("bodyspool: stat spool directory %s: %w", dir, err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("bodyspool: spool directory %s must be a directory with mode 0700, got %v", dir, info.Mode())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("bodyspool: read spool directory %s: %w", dir, err)
	}
	for _, e := range entries {
		if e.Type().IsRegular() {
			return fmt.Errorf("bodyspool: spool directory %s holds a regular file %q; spools are anonymous, remove it", dir, e.Name())
		}
	}
	f, err := openTmpFile(dir)
	if err != nil {
		return fmt.Errorf("bodyspool: spool filesystem rejects O_TMPFILE: %w", err)
	}
	return f.Close()
}
