package storage

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
)

func TestBtrfsHealthStatusIgnoresUnrelatedMissingText(t *testing.T) {
	oldRunBtrfsCommand := RunBtrfsCommand
	t.Cleanup(func() {
		RunBtrfsCommand = oldRunBtrfsCommand
	})

	RunBtrfsCommand = func(_ context.Context, args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "filesystem" && args[1] == "show" {
			return "Label: Missing_Data\n", nil
		}
		return "[/dev/md0].write_io_errs 0\n[/dev/md0].read_io_errs 0\n", nil
	}

	got, ok := btrfsHealthStatus(context.Background(), "/srv")
	if !ok || got != "DEVICE STATS CLEAN" {
		t.Fatalf("btrfsHealthStatus() = %q, %v, want %q, true", got, ok, "DEVICE STATS CLEAN")
	}
}

func TestBtrfsHealthStatusDoesNotTreatCommandFailureAsDeviceErrors(t *testing.T) {
	oldRunBtrfsCommand := RunBtrfsCommand
	t.Cleanup(func() {
		RunBtrfsCommand = oldRunBtrfsCommand
	})

	RunBtrfsCommand = func(_ context.Context, args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "filesystem" && args[1] == "show" {
			return "Label: srv\n", nil
		}
		return "ERROR: not a mounted btrfs device", errors.New("exit status 1")
	}

	got, ok := btrfsHealthStatus(context.Background(), "/srv")
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
