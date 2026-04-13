package host

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"srv/internal/format"
)

type CapacitySummary struct {
	Hostname  string             `json:"hostname"`
	OS        OSInfo             `json:"os"`
	CPU       CPUInfo            `json:"cpu"`
	Instances CapacityInstances  `json:"instances"`
	Capacity  []CapacityResource `json:"capacity"`
}

type OSInfo struct {
	Name   string `json:"name"`
	Kernel string `json:"kernel"`
	Arch   string `json:"arch"`
}

type CPUInfo struct {
	Model         string  `json:"model"`
	PhysicalCores int     `json:"physical_cores"`
	LogicalCores  int     `json:"logical_cores"`
	Load1m        float64 `json:"load_1m"`
	Load5m        float64 `json:"load_5m"`
	Load15m       float64 `json:"load_15m"`
}

type CapacityInstances struct {
	Total   int            `json:"total"`
	Running int            `json:"running"`
	Stopped int            `json:"stopped"`
	Failed  int            `json:"failed"`
	ByState map[string]int `json:"by_state,omitempty"`
}

type CapacityResource struct {
	Resource  string           `json:"resource"`
	Unit      string           `json:"unit"`
	Allocated int64            `json:"allocated"`
	Budget    int64            `json:"budget"`
	Left      int64            `json:"left"`
	Total     int64            `json:"total,omitempty"`
	Reserve   int64            `json:"reserve,omitempty"`
	Advisory  bool             `json:"advisory,omitempty"`
	Note      string           `json:"note,omitempty"`
	Details   []CapacityDetail `json:"details,omitempty"`
}

type CapacityDetail struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

var readFile = func(path string) ([]byte, error) { return os.ReadFile(path) }

func DefaultReadHostMemoryBytes() (int64, error) {
	return LoadHostMemoryBytes("/proc/meminfo")
}

func DefaultReadFilesystemBytes(path string) (int64, error) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return 0, err
	}
	return int64(fs.Blocks) * int64(fs.Bsize), nil
}

func LoadHostMemoryBytes(path string) (int64, error) {
	payload, err := readFile(path)
	if err != nil {
		return 0, err
	}
	var totalKiB int64
	for _, line := range strings.Split(string(payload), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "MemTotal:" {
			continue
		}
		value, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, err
		}
		totalKiB = value
		break
	}
	if totalKiB == 0 {
		return 0, fmt.Errorf("MemTotal missing from %s", path)
	}
	return totalKiB * format.KiB, nil
}

func ReadOSInfo() OSInfo {
	info := OSInfo{
		Arch: runtime.GOARCH,
	}

	data, err := readFile("/etc/os-release")
	if err == nil {
		info.Name = parseOSRelease(string(data))
	}

	var uname syscall.Utsname
	if err := syscall.Uname(&uname); err == nil {
		info.Kernel = int8ToString(uname.Release[:])
	}

	return info
}

func parseOSRelease(data string) string {
	var name, version string
	for _, line := range strings.Split(data, "\n") {
		if strings.HasPrefix(line, "PRETTY_NAME=") {
			return strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), `"`)
		}
		if strings.HasPrefix(line, "NAME=") {
			name = strings.Trim(strings.TrimPrefix(line, "NAME="), `"`)
		}
		if strings.HasPrefix(line, "VERSION=") {
			version = strings.Trim(strings.TrimPrefix(line, "VERSION="), `"`)
		}
	}
	if name != "" && version != "" {
		return name + " " + version
	}
	return name
}

func int8ToString(arr []int8) string {
	b := make([]byte, 0, len(arr))
	for _, v := range arr {
		if v == 0 {
			break
		}
		b = append(b, byte(v))
	}
	return string(b)
}

func ReadCPUInfo() CPUInfo {
	info := CPUInfo{
		LogicalCores: runtime.NumCPU(),
	}

	data, err := readFile("/proc/cpuinfo")
	if err != nil {
		return info
	}

	modelNames := make(map[string]struct{})
	physicalIDs := make(map[string]struct{})
	coreIDs := make(map[string]struct{})

	var currentPhysicalID, currentCoreID string

	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, "model name") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				modelNames[strings.TrimSpace(parts[1])] = struct{}{}
			}
		}
		if strings.Contains(line, "physical id") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				currentPhysicalID = strings.TrimSpace(parts[1])
				physicalIDs[currentPhysicalID] = struct{}{}
			}
		}
		if strings.Contains(line, "core id") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				currentCoreID = strings.TrimSpace(parts[1])
				key := currentPhysicalID + ":" + currentCoreID
				coreIDs[key] = struct{}{}
			}
		}
	}

	if len(modelNames) == 1 {
		for name := range modelNames {
			info.Model = name
		}
	}

	if len(coreIDs) > 0 {
		info.PhysicalCores = len(coreIDs)
	} else {
		info.PhysicalCores = len(physicalIDs)
		if info.PhysicalCores == 0 {
			info.PhysicalCores = info.LogicalCores
		}
	}

	return info
}

func ReadLoadAvg() (float64, float64, float64) {
	data, err := readFile("/proc/loadavg")
	if err != nil {
		return 0, 0, 0
	}

	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return 0, 0, 0
	}

	load1m, _ := strconv.ParseFloat(fields[0], 64)
	load5m, _ := strconv.ParseFloat(fields[1], 64)
	load15m, _ := strconv.ParseFloat(fields[2], 64)

	return load1m, load5m, load15m
}
