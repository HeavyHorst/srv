package storage

import (
	"os"
	"reflect"
	"testing"
)

func TestBtrfsHealthStatusReportsCleanFromSysfs(t *testing.T) {
	oldReadDirNames := ReadDirNames
	oldReadTrimmedFile := ReadTrimmedFile
	oldEvalSymlinks := EvalSymlinks
	t.Cleanup(func() {
		ReadDirNames = oldReadDirNames
		ReadTrimmedFile = oldReadTrimmedFile
		EvalSymlinks = oldEvalSymlinks
	})

	EvalSymlinks = func(path string) (string, error) {
		if path == "/dev/mapper/cryptroot" {
			return "/dev/dm-0", nil
		}
		return path, nil
	}
	ReadDirNames = func(path string) ([]string, error) {
		switch path {
		case "/sys/fs/btrfs":
			return []string{"features", "btrfs-control", "fsid-1"}, nil
		case "/sys/fs/btrfs/fsid-1/devices":
			return []string{"dm-7", "dm-0"}, nil
		case "/sys/fs/btrfs/fsid-1/devinfo":
			return []string{"1"}, nil
		default:
			return nil, os.ErrNotExist
		}
	}
	ReadTrimmedFile = func(path string) (string, error) {
		switch path {
		case "/sys/class/block/dm-0/dev":
			return "253:0", nil
		case "/sys/class/block/dm-7/dev":
			return "253:7", nil
		case "/sys/fs/btrfs/fsid-1/devinfo/1/missing":
			return "0", nil
		case "/sys/fs/btrfs/fsid-1/devinfo/1/error_stats":
			return "write_errs 0\nread_errs 0\nflush_errs 0\ncorruption_errs 0\ngeneration_errs 0", nil
		default:
			return "", os.ErrNotExist
		}
	}

	got, ok := btrfsHealthStatus("/dev/mapper/cryptroot")
	if !ok || got != "DEVICE STATS CLEAN" {
		t.Fatalf("btrfsHealthStatus() = %q, %v, want %q, true", got, ok, "DEVICE STATS CLEAN")
	}
}

func TestBtrfsHealthStatusReportsDeviceErrorsFromSysfs(t *testing.T) {
	oldReadDirNames := ReadDirNames
	oldReadTrimmedFile := ReadTrimmedFile
	oldEvalSymlinks := EvalSymlinks
	t.Cleanup(func() {
		ReadDirNames = oldReadDirNames
		ReadTrimmedFile = oldReadTrimmedFile
		EvalSymlinks = oldEvalSymlinks
	})

	EvalSymlinks = func(path string) (string, error) { return path, nil }
	ReadDirNames = func(path string) ([]string, error) {
		switch path {
		case "/sys/fs/btrfs":
			return []string{"fsid-1"}, nil
		case "/sys/fs/btrfs/fsid-1/devices":
			return []string{"md0"}, nil
		case "/sys/fs/btrfs/fsid-1/devinfo":
			return []string{"1"}, nil
		default:
			return nil, os.ErrNotExist
		}
	}
	ReadTrimmedFile = func(path string) (string, error) {
		switch path {
		case "/sys/class/block/md0/dev":
			return "9:0", nil
		case "/sys/fs/btrfs/fsid-1/devinfo/1/missing":
			return "0", nil
		case "/sys/fs/btrfs/fsid-1/devinfo/1/error_stats":
			return "write_errs 0\nread_errs 1\nflush_errs 0\ncorruption_errs 0\ngeneration_errs 0", nil
		default:
			return "", os.ErrNotExist
		}
	}

	got, ok := btrfsHealthStatus("/dev/md0")
	if !ok || got != "DEVICE ERRORS" {
		t.Fatalf("btrfsHealthStatus() = %q, %v, want %q, true", got, ok, "DEVICE ERRORS")
	}
}

func TestBtrfsHealthStatusReportsDegradedFromSysfs(t *testing.T) {
	oldReadDirNames := ReadDirNames
	oldReadTrimmedFile := ReadTrimmedFile
	oldEvalSymlinks := EvalSymlinks
	t.Cleanup(func() {
		ReadDirNames = oldReadDirNames
		ReadTrimmedFile = oldReadTrimmedFile
		EvalSymlinks = oldEvalSymlinks
	})

	EvalSymlinks = func(path string) (string, error) { return path, nil }
	ReadDirNames = func(path string) ([]string, error) {
		switch path {
		case "/sys/fs/btrfs":
			return []string{"fsid-1"}, nil
		case "/sys/fs/btrfs/fsid-1/devices":
			return []string{"md0"}, nil
		case "/sys/fs/btrfs/fsid-1/devinfo":
			return []string{"1", "2"}, nil
		default:
			return nil, os.ErrNotExist
		}
	}
	ReadTrimmedFile = func(path string) (string, error) {
		switch path {
		case "/sys/class/block/md0/dev":
			return "9:0", nil
		case "/sys/fs/btrfs/fsid-1/devinfo/1/missing":
			return "0", nil
		case "/sys/fs/btrfs/fsid-1/devinfo/1/error_stats":
			return "write_errs 0\nread_errs 0\nflush_errs 0\ncorruption_errs 0\ngeneration_errs 0", nil
		case "/sys/fs/btrfs/fsid-1/devinfo/2/missing":
			return "1", nil
		default:
			return "", os.ErrNotExist
		}
	}

	got, ok := btrfsHealthStatus("/dev/md0")
	if !ok || got != "DEGRADED" {
		t.Fatalf("btrfsHealthStatus() = %q, %v, want %q, true", got, ok, "DEGRADED")
	}
}

func TestBtrfsHealthStatusReturnsUnavailableWhenSourceDeviceDoesNotMatchFSID(t *testing.T) {
	oldReadDirNames := ReadDirNames
	oldReadTrimmedFile := ReadTrimmedFile
	oldEvalSymlinks := EvalSymlinks
	t.Cleanup(func() {
		ReadDirNames = oldReadDirNames
		ReadTrimmedFile = oldReadTrimmedFile
		EvalSymlinks = oldEvalSymlinks
	})

	EvalSymlinks = func(path string) (string, error) { return path, nil }
	ReadDirNames = func(path string) ([]string, error) {
		switch path {
		case "/sys/fs/btrfs":
			return []string{"features", "fsid-1"}, nil
		case "/sys/fs/btrfs/fsid-1/devices":
			return []string{"md1"}, nil
		default:
			return nil, os.ErrNotExist
		}
	}
	ReadTrimmedFile = func(path string) (string, error) {
		switch path {
		case "/sys/class/block/md0/dev":
			return "9:0", nil
		case "/sys/class/block/md1/dev":
			return "9:1", nil
		default:
			return "", os.ErrNotExist
		}
	}

	got, ok := btrfsHealthStatus("/dev/md0")
	if ok || got != "" {
		t.Fatalf("btrfsHealthStatus() = %q, %v, want empty/false", got, ok)
	}
}

func TestMDDevicesForSourceFollowsDeviceMapperSlaves(t *testing.T) {
	oldEvalSymlinks := EvalSymlinks
	oldReadDirNames := ReadDirNames
	oldPathExists := PathExists
	t.Cleanup(func() {
		EvalSymlinks = oldEvalSymlinks
		ReadDirNames = oldReadDirNames
		PathExists = oldPathExists
	})

	EvalSymlinks = func(path string) (string, error) {
		if path == "/dev/mapper/cryptroot" {
			return "/dev/dm-0", nil
		}
		if path == "/sys/class/block/md0p1" {
			return "/sys/devices/virtual/block/md0/md0p1", nil
		}
		return path, nil
	}
	ReadDirNames = func(path string) ([]string, error) {
		switch path {
		case "/sys/class/block/dm-0/slaves":
			return []string{"md0p1"}, nil
		case "/sys/class/block/md0/slaves":
			return nil, os.ErrNotExist
		default:
			return nil, os.ErrNotExist
		}
	}
	PathExists = func(path string) bool {
		return path == "/sys/class/block/md0p1/partition"
	}

	got, err := mdDevicesForSource("/dev/mapper/cryptroot")
	if err != nil {
		t.Fatalf("mdDevicesForSource(): %v", err)
	}
	if !reflect.DeepEqual(got, []string{"md0"}) {
		t.Fatalf("mdDevicesForSource() = %#v, want %#v", got, []string{"md0"})
	}
}
