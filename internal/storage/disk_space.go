package storage

import (
	"fmt"
	"math"

	"golang.org/x/sys/unix"
)

// DiskRuntimeFloorBytes is an initial host policy, not a guarantee against
// ENOSPC. Guests, COW metadata and unrelated host writers can consume space.
const DiskRuntimeFloorBytes int64 = 1 << 30

// CheckAvailableSpace compares measured user-available bytes with the floor
// and this operation's estimated payload. Reflink clones are not full copies.
func CheckAvailableSpace(available, payload int64) error {
	if payload < 0 || available < DiskRuntimeFloorBytes || payload > available-DiskRuntimeFloorBytes {
		return fmt.Errorf("insufficient available disk space: %d bytes available, %d runtime floor, %d estimated payload", available, DiskRuntimeFloorBytes, payload)
	}
	return nil
}

// CheckDiskSpace must be called after acquiring the payload lock for a bulk
// writer. Logical payload length is an estimate, not a Btrfs allocation bound.
func CheckDiskSpace(path string, payload int64) error {
	var fs unix.Statfs_t
	if err := unix.Statfs(path, &fs); err != nil {
		return fmt.Errorf("measure available disk space at %s: %w", path, err)
	}
	if fs.Bsize <= 0 || fs.Bavail > uint64(math.MaxInt64)/uint64(fs.Bsize) {
		return fmt.Errorf("invalid available disk measurement at %s", path)
	}
	return CheckAvailableSpace(int64(fs.Bavail)*fs.Bsize, payload)
}
