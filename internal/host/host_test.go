package host

import (
	"testing"

	"srv/internal/format"
)

func TestReadOSInfo(t *testing.T) {
	info := ReadOSInfo()
	if info.Arch == "" {
		t.Fatal("ReadOSInfo() Arch is empty")
	}
}

func TestReadCPUInfo(t *testing.T) {
	info := ReadCPUInfo()
	if info.LogicalCores == 0 {
		t.Fatal("ReadCPUInfo() LogicalCores is zero")
	}
}

func TestReadLoadAvg(t *testing.T) {
	load1m, load5m, load15m := ReadLoadAvg()
	if load1m < 0 || load5m < 0 || load15m < 0 {
		t.Fatalf("ReadLoadAvg() = %f, %f, %f, want non-negative", load1m, load5m, load15m)
	}
}

func TestDefaultReadHostMemoryBytes(t *testing.T) {
	total, err := DefaultReadHostMemoryBytes()
	if err != nil {
		t.Fatalf("DefaultReadHostMemoryBytes() error = %v", err)
	}
	if total <= 0 {
		t.Fatalf("DefaultReadHostMemoryBytes() = %d, want positive", total)
	}
}

func TestDefaultReadFilesystemBytes(t *testing.T) {
	total, err := DefaultReadFilesystemBytes("/")
	if err != nil {
		t.Fatalf("DefaultReadFilesystemBytes() error = %v", err)
	}
	if total <= 0 {
		t.Fatalf("DefaultReadFilesystemBytes() = %d, want positive", total)
	}
}

func TestLoadHostMemoryBytesParsesMemTotal(t *testing.T) {
	oldReadFile := readFile
	readFile = func(path string) ([]byte, error) {
		return []byte("MemTotal:       16384000 kB\nMemFree:          8000000 kB\n"), nil
	}
	t.Cleanup(func() { readFile = oldReadFile })

	total, err := LoadHostMemoryBytes("/proc/meminfo")
	if err != nil {
		t.Fatalf("LoadHostMemoryBytes() error = %v", err)
	}
	if total != 16384000*format.KiB {
		t.Fatalf("LoadHostMemoryBytes() = %d, want %d", total, 16384000*format.KiB)
	}
}

func TestLoadHostMemoryBytesMissingMemTotal(t *testing.T) {
	oldReadFile := readFile
	readFile = func(path string) ([]byte, error) {
		return []byte("MemFree:          8000000 kB\n"), nil
	}
	t.Cleanup(func() { readFile = oldReadFile })

	_, err := LoadHostMemoryBytes("/proc/meminfo")
	if err == nil {
		t.Fatal("LoadHostMemoryBytes() expected error for missing MemTotal")
	}
}

func TestParseOSRelease(t *testing.T) {
	data := "NAME=\"Ubuntu\"\nVERSION=\"22.04 LTS (Jammy Jellyfish)\""
	got := parseOSRelease(data)
	if got != "Ubuntu 22.04 LTS (Jammy Jellyfish)" {
		t.Fatalf("parseOSRelease() = %q, want %q", got, "Ubuntu 22.04 LTS (Jammy Jellyfish)")
	}

	data2 := "PRETTY_NAME=\"Debian GNU/Linux 12 (bookworm)\""
	got2 := parseOSRelease(data2)
	if got2 != "Debian GNU/Linux 12 (bookworm)" {
		t.Fatalf("parseOSRelease() = %q, want %q", got2, "Debian GNU/Linux 12 (bookworm)")
	}
}

func TestReadCPUInfoParsesModel(t *testing.T) {
	oldReadFile := readFile
	readFile = func(path string) ([]byte, error) {
		return []byte("processor\t: 0\nmodel name\t: Test CPU\nphysical id\t: 0\ncore id\t\t: 0\n"), nil
	}
	t.Cleanup(func() { readFile = oldReadFile })

	info := ReadCPUInfo()
	if info.Model != "Test CPU" {
		t.Fatalf("ReadCPUInfo() Model = %q, want %q", info.Model, "Test CPU")
	}
}

func TestReadLoadAvgParsesValues(t *testing.T) {
	oldReadFile := readFile
	readFile = func(path string) ([]byte, error) {
		return []byte("0.10 0.20 0.30 1/234 56789\n"), nil
	}
	t.Cleanup(func() { readFile = oldReadFile })

	load1m, load5m, load15m := ReadLoadAvg()
	if load1m != 0.10 || load5m != 0.20 || load15m != 0.30 {
		t.Fatalf("ReadLoadAvg() = %f, %f, %f, want 0.10, 0.20, 0.30", load1m, load5m, load15m)
	}
}
