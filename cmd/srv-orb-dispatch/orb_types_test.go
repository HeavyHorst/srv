package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func writeOrbTypes(t *testing.T, h harness, data string) string {
	t.Helper()
	path := filepath.Join(h.dir, "types.json")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDispatchOrbTypes(t *testing.T) {
	shapes := []orbResources{{1, 2048}, {2, 4096}, {4, 8192}, {8, 16384}, {16, 32768}, {16, 45056}}
	for _, shape := range shapes {
		t.Run(strconv.FormatInt(shape.CPUs, 10)+"-"+strconv.FormatInt(shape.MemoryMiB, 10), func(t *testing.T) {
			h := newHarness(t, "")
			data, err := json.Marshal(map[string]orbResources{"selected": shape})
			if err != nil {
				t.Fatal(err)
			}
			path := writeOrbTypes(t, h, string(data))
			h.must("spawn", "first", "ORB_TYPE=selected", "SRV_ORB_TYPES_FILE="+path, "SRV_ORB_DISK=20G")
			s := h.read()
			if s.CPUs != strconv.FormatInt(shape.CPUs, 10) || s.RAM != strconv.FormatInt(shape.MemoryMiB, 10) || s.RootFS != "20G" || s.Creates != 1 {
				t.Fatalf("wrong creation resources: %+v", s)
			}
			id := s.VM.ID
			// Mapping changes, removal, and an absent ORB_TYPE on pause must not
			// affect saved-state pause or the resource shape of this incarnation.
			writeOrbTypes(t, h, `{"selected":{"cpus":2,"memory_mib":4096}}`)
			h.must("pause", "", "SRV_ORB_TYPES_FILE="+path)
			h.must("spawn", "second", "ORB_TYPE=selected", "SRV_ORB_TYPES_FILE="+path)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			h.must("pause", "", "SRV_ORB_TYPES_FILE="+path)
			h.must("spawn", "third", "ORB_TYPE=selected", "SRV_ORB_TYPES_FILE="+path)
			s = h.read()
			if s.Creates != 1 || s.VM.ID != id || s.CPUs != strconv.FormatInt(shape.CPUs, 10) || s.RAM != strconv.FormatInt(shape.MemoryMiB, 10) || s.SaveStops != 2 || s.Restores != 2 {
				t.Fatalf("resume changed allocation: %+v", s)
			}
			h.must("delete", "", "SRV_ORB_TYPES_FILE="+path)
		})
	}
}

func TestDispatchOrbTypeAdmission(t *testing.T) {
	cases := map[string]string{
		"unknown":            `{"other":{"cpus":2,"memory_mib":4096}}`,
		"malformed":          `{`,
		"invalid cpu":        `{"selected":{"cpus":3,"memory_mib":4096}}`,
		"invalid memory":     `{"selected":{"cpus":2,"memory_mib":127}}`,
		"invalid unselected": `{"selected":{"cpus":2,"memory_mib":4096},"other":{"cpus":0,"memory_mib":4096}}`,
		"unknown field":      `{"selected":{"cpus":2,"memory_mib":4096,"disk":20}}`,
		"missing memory":     `{"selected":{"cpus":2}}`,
		"null":               `null`,
		"trailing":           `{"selected":{"cpus":2,"memory_mib":4096}} {}`,
		"no file":            "",
		"relative file":      "relative",
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, "")
			path := ""
			if data == "relative" {
				path = data
			} else if data != "" {
				path = writeOrbTypes(t, h, data)
			}
			if out, err := h.run("spawn", "secret", "ORB_TYPE=selected", "SRV_ORB_TYPES_FILE="+path); err == nil {
				t.Fatalf("invalid mapping accepted: %s", out)
			}
			s := h.read()
			if s.CreateAttempts != 0 || s.VM != nil {
				t.Fatalf("invalid mapping allocated: %+v", s)
			}
			files, err := filepath.Glob(filepath.Join(h.dir, "state", "orb-*.json"))
			if err != nil {
				t.Fatal(err)
			}
			for _, file := range files {
				b, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(b), `"phase": "creating"`) {
					t.Fatalf("invalid mapping fenced as creating: %s", b)
				}
			}
		})
	}
}

func TestDispatchUntypedOrbResources(t *testing.T) {
	h := newHarness(t, "")
	h.must("spawn", "secret", "SRV_ORB_CPUS=8", "SRV_ORB_RAM=12G", "SRV_ORB_TYPES_FILE=/missing/types.json")
	s := h.read()
	if s.CPUs != "8" || s.RAM != "12G" {
		t.Fatalf("untyped defaults changed: %+v", s)
	}
}

func TestDispatchTypedSnapshotClone(t *testing.T) {
	h := newHarness(t, "")
	path := writeOrbTypes(t, h, `{"selected":{"cpus":4,"memory_mib":8192}}`)
	typed := []string{"ORB_TYPE=selected", "SRV_ORB_TYPES_FILE=" + path, "ORB_SNAPSHOT_ID=typed-project"}
	h.must("spawn", "first", typed...)
	s := h.read()
	s.Disk = "prepared project"
	h.write(s)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	h.must("pause", "", "ORB_SNAPSHOT_ID=typed-project", "SRV_ORB_TYPES_FILE="+path)
	if s = h.read(); s.SaveStops != 0 || len(s.Templates) != 1 {
		t.Fatalf("snapshot pause semantics changed: %+v", s)
	}
	h.must("delete", "", "SRV_ORB_TYPES_FILE="+path)
	writeOrbTypes(t, h, `{"selected":{"cpus":8,"memory_mib":16384}}`)
	h.must("spawn", "clone", typed...)
	s = h.read()
	if s.Creates != 2 || s.CPUs != "8" || s.RAM != "16384" || s.Disk != "prepared project" {
		t.Fatalf("typed clone lost resources or project: %+v", s)
	}
}
