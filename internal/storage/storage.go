package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Detail struct {
	Label string
	Value string
}

type MountInfo struct {
	MountPoint string
	FSType     string
	Source     string
}

var (
	ReadProcMountInfo = func() ([]byte, error) { return os.ReadFile("/proc/self/mountinfo") }
	ReadTrimmedFile   = func(path string) (string, error) {
		payload, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(payload)), nil
	}
	ReadDirNames = func(path string) ([]string, error) {
		entries, err := os.ReadDir(path)
		if err != nil {
			return nil, err
		}
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		return names, nil
	}
	PathExists   = func(path string) bool { _, err := os.Stat(path); return err == nil }
	EvalSymlinks = filepath.EvalSymlinks
)

func StorageDetails(ctx context.Context, path string) []Detail {
	mount, err := MountInfoForPath(path)
	if err != nil {
		return nil
	}

	details := make([]Detail, 0, 2)
	if mount.FSType == "btrfs" {
		if value, ok := btrfsHealthStatus(mount.Source); ok {
			details = append(details, Detail{Label: "BTRFS", Value: value})
		}
	}
	if value, ok := MDADMHealthStatus(mount.Source); ok {
		details = append(details, Detail{Label: "MDADM", Value: value})
	}
	return details
}

func MountInfoForPath(path string) (MountInfo, error) {
	path = filepath.Clean(path)
	payload, err := ReadProcMountInfo()
	if err != nil {
		return MountInfo{}, err
	}

	best := MountInfo{}
	bestLen := -1
	for _, line := range strings.Split(string(payload), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		mount, err := parseMountInfoLine(line)
		if err != nil {
			continue
		}
		if !mountPointContainsPath(mount.MountPoint, path) {
			continue
		}
		if l := len(mount.MountPoint); l > bestLen {
			best = mount
			bestLen = l
		}
	}
	if bestLen < 0 {
		return MountInfo{}, fmt.Errorf("no mount for %s", path)
	}
	return best, nil
}

func parseMountInfoLine(line string) (MountInfo, error) {
	parts := strings.SplitN(line, " - ", 2)
	if len(parts) != 2 {
		return MountInfo{}, errors.New("invalid mountinfo line")
	}
	before := strings.Fields(parts[0])
	after := strings.Fields(parts[1])
	if len(before) < 5 || len(after) < 2 {
		return MountInfo{}, errors.New("short mountinfo line")
	}
	return MountInfo{
		MountPoint: unescapeMountInfoValue(before[4]),
		FSType:     after[0],
		Source:     unescapeMountInfoValue(after[1]),
	}, nil
}

func mountPointContainsPath(mountPoint, path string) bool {
	if mountPoint == "/" {
		return strings.HasPrefix(path, "/")
	}
	if path == mountPoint {
		return true
	}
	return strings.HasPrefix(path, mountPoint+string(os.PathSeparator))
}

func unescapeMountInfoValue(value string) string {
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] == '\\' && i+3 < len(value) {
			decoded, err := strconv.ParseInt(value[i+1:i+4], 8, 32)
			if err == nil {
				b.WriteByte(byte(decoded))
				i += 3
				continue
			}
		}
		b.WriteByte(value[i])
	}
	return b.String()
}

func btrfsHealthStatus(source string) (string, bool) {
	fsid, err := btrfsFSIDForMountSource(source)
	if err != nil || fsid == "" {
		return "", false
	}
	deviceIDs, err := ReadDirNames(filepath.Join("/sys/fs/btrfs", fsid, "devinfo"))
	if err != nil {
		return "", false
	}

	hasStats := false
	hasErrors := false
	for _, deviceID := range deviceIDs {
		base := filepath.Join("/sys/fs/btrfs", fsid, "devinfo", deviceID)
		missing, err := ReadTrimmedFile(filepath.Join(base, "missing"))
		if err == nil && missing == "1" {
			return "DEGRADED", true
		}
		stats, err := ReadTrimmedFile(filepath.Join(base, "error_stats"))
		if err != nil {
			continue
		}
		deviceHasStats, deviceHasErrors := parseBtrfsDeviceStatsOutput(stats)
		hasStats = hasStats || deviceHasStats
		hasErrors = hasErrors || deviceHasErrors
	}
	if hasErrors {
		return "DEVICE ERRORS", true
	}
	if hasStats {
		return "DEVICE STATS CLEAN", true
	}
	return "", false
}

func btrfsFSIDForMountSource(source string) (string, error) {
	deviceID, err := blockDeviceID(source)
	if err != nil || deviceID == "" {
		return "", err
	}
	fsids, err := ReadDirNames("/sys/fs/btrfs")
	if err != nil {
		return "", err
	}
	for _, fsid := range fsids {
		if fsid == "features" || fsid == "btrfs-control" {
			continue
		}
		devices, err := ReadDirNames(filepath.Join("/sys/fs/btrfs", fsid, "devices"))
		if err != nil {
			continue
		}
		for _, device := range devices {
			candidateID, err := ReadTrimmedFile(filepath.Join("/sys/class/block", device, "dev"))
			if err != nil || candidateID == "" {
				continue
			}
			if candidateID == deviceID {
				return fsid, nil
			}
		}
	}
	return "", nil
}

func blockDeviceID(source string) (string, error) {
	deviceName, err := blockDeviceName(source)
	if err != nil || deviceName == "" {
		return "", err
	}
	return ReadTrimmedFile(filepath.Join("/sys/class/block", deviceName, "dev"))
}

func parseBtrfsDeviceStatsOutput(output string) (bool, bool) {
	hasStats := false
	hasErrors := false
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseInt(fields[len(fields)-1], 10, 64)
		if err != nil {
			continue
		}
		hasStats = true
		if value != 0 {
			hasErrors = true
		}
	}
	return hasStats, hasErrors
}

func MDADMHealthStatus(source string) (string, bool) {
	devices, err := mdDevicesForSource(source)
	if err != nil || len(devices) == 0 {
		return "", false
	}

	statuses := make([]string, 0, len(devices))
	for _, device := range devices {
		status, err := mdDeviceHealthStatus(device)
		if err != nil {
			return "", false
		}
		if len(devices) == 1 {
			statuses = append(statuses, status)
			continue
		}
		statuses = append(statuses, device+": "+status)
	}
	return strings.Join(statuses, "; "), true
}

func mdDevicesForSource(source string) ([]string, error) {
	if source == "" || source == "none" {
		return nil, nil
	}
	name, err := blockDeviceName(source)
	if err != nil {
		return nil, err
	}
	if name == "" {
		return nil, nil
	}
	devices, err := collectMDDevices(name, make(map[string]bool))
	if err != nil {
		return nil, err
	}
	return devices, nil
}

func blockDeviceName(source string) (string, error) {
	resolved := source
	if strings.HasPrefix(source, "/dev/") {
		path, err := EvalSymlinks(source)
		if err == nil {
			resolved = path
		}
	}
	return filepath.Base(resolved), nil
}

func collectMDDevices(name string, seen map[string]bool) ([]string, error) {
	if name == "" {
		return nil, nil
	}
	normalized, err := normalizeBlockDeviceName(name)
	if err != nil {
		return nil, err
	}
	if normalized == "" || seen[normalized] {
		return nil, nil
	}
	name = normalized
	seen[name] = true
	if strings.HasPrefix(name, "md") {
		return []string{name}, nil
	}

	slaves, err := ReadDirNames(filepath.Join("/sys/class/block", name, "slaves"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	devices := make([]string, 0, len(slaves))
	for _, slave := range slaves {
		nested, err := collectMDDevices(slave, seen)
		if err != nil {
			return nil, err
		}
		devices = append(devices, nested...)
	}
	return devices, nil
}

func normalizeBlockDeviceName(name string) (string, error) {
	partitionPath := filepath.Join("/sys/class/block", name, "partition")
	if !PathExists(partitionPath) {
		return name, nil
	}
	resolved, err := EvalSymlinks(filepath.Join("/sys/class/block", name))
	if err != nil {
		return "", err
	}
	parent := filepath.Base(filepath.Dir(resolved))
	if parent == "" || parent == "." || parent == string(filepath.Separator) {
		return name, nil
	}
	return parent, nil
}

func mdDeviceHealthStatus(device string) (string, error) {
	base := filepath.Join("/sys/class/block", device, "md")
	arrayState, err := ReadTrimmedFile(filepath.Join(base, "array_state"))
	if err != nil {
		return "", err
	}
	degraded, err := ReadTrimmedFile(filepath.Join(base, "degraded"))
	if err != nil {
		return "", err
	}
	degradedCount, err := strconv.Atoi(degraded)
	if err != nil {
		return "", err
	}
	if degradedCount > 0 {
		return "DEGRADED", nil
	}
	if syncAction, err := ReadTrimmedFile(filepath.Join(base, "sync_action")); err == nil {
		switch syncAction {
		case "resync":
			return "RESYNCING", nil
		case "recover":
			return "RECOVERING", nil
		case "reshape":
			return "RESHAPING", nil
		case "repair":
			return "REPAIRING", nil
		case "check":
			return "CHECKING", nil
		}
	}
	if strings.Contains(arrayState, "clean") || arrayState == "active" || arrayState == "readonly" || arrayState == "read-auto" {
		return "HEALTH O.K.", nil
	}
	return strings.ToUpper(strings.ReplaceAll(arrayState, "-", " ")), nil
}
