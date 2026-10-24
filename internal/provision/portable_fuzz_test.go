package provision

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"srv/internal/model"
)

// validPortableManifest mirrors what ExportInstance writes for a fixed-memory VM.
func validPortableManifest() portableManifest {
	return portableManifest{
		Version:    portableManifestVersion,
		ExportedAt: time.Date(2026, 10, 22, 1, 2, 3, 0, time.UTC),
		Instance: portableInstance{
			Kind:            model.KindVM,
			MemoryMode:      model.MemoryModeFixed,
			ID:              "instance-id",
			Name:            "example",
			Image:           "arch",
			CreatedAt:       time.Date(2026, 10, 21, 1, 2, 3, 0, time.UTC),
			CreatedByUser:   "alice@example.com",
			CreatedByNode:   "laptop",
			VCPUCount:       2,
			MemoryMiB:       2048,
			RootFSSizeBytes: 1 << 30,
		},
		Files: backupFiles{RootFS: backupRootFSName},
	}
}

func tarWithNamedPayload(tb testing.TB, name string, typeflag byte, payload []byte) []byte {
	tb.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     0o640,
		Size:     int64(len(payload)),
		Typeflag: typeflag,
	}); err != nil {
		tb.Fatal(err)
	}
	if _, err := tw.Write(payload); err != nil {
		tb.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		tb.Fatal(err)
	}
	return buf.Bytes()
}

func tarWithNamedManifest(tb testing.TB, name string, manifest portableManifest) []byte {
	tb.Helper()
	payload, err := json.Marshal(manifest)
	if err != nil {
		tb.Fatal(err)
	}
	return tarWithNamedPayload(tb, name, tar.TypeReg, payload)
}

// rawTarEntry builds a header archive/tar.Writer refuses to write: a
// non-regular entry that still carries a payload. It makes the type guard
// deterministically testable instead of relying on byte mutation.
func rawTarEntry(tb testing.TB, name string, typeflag byte, payload []byte) []byte {
	tb.Helper()
	header := make([]byte, 512)
	copy(header[0:100], name)
	copy(header[100:108], "0000640\x00")
	copy(header[108:116], "0000000\x00")
	copy(header[116:124], "0000000\x00")
	copy(header[124:136], fmt.Sprintf("%011o\x00", len(payload)))
	copy(header[136:148], "00000000000\x00")
	for i := 148; i < 156; i++ {
		header[i] = ' '
	}
	header[156] = typeflag
	copy(header[257:263], "ustar\x00")
	copy(header[263:265], "00")
	sum := 0
	for _, b := range header {
		sum += int(b)
	}
	copy(header[148:156], fmt.Sprintf("%06o\x00 ", sum))

	out := append([]byte(nil), header...)
	out = append(out, payload...)
	if remainder := len(payload) % 512; remainder != 0 {
		out = append(out, make([]byte, 512-remainder)...)
	}
	return append(out, make([]byte, 1024)...)
}

// Positive controls keep the fuzz oracles from passing when validation rejects
// every input.
func TestPortableManifestValidateAcceptsExport(t *testing.T) {
	if err := validPortableManifest().validate(); err != nil {
		t.Fatalf("exported manifest rejected: %v", err)
	}
}

func TestReadPortableManifestAcceptsExport(t *testing.T) {
	manifest, err := readPortableManifest(tar.NewReader(bytes.NewReader(tarWithNamedManifest(t, backupManifestName, validPortableManifest()))))
	if err != nil {
		t.Fatalf("exported manifest rejected: %v", err)
	}
	if manifest.Instance.Name != "example" {
		t.Fatalf("manifest name = %q, want example", manifest.Instance.Name)
	}
}

func TestReadPortableManifestRejectsNonRegularAndOversized(t *testing.T) {
	payload, err := json.Marshal(validPortableManifest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readPortableManifest(tar.NewReader(bytes.NewReader(rawTarEntry(t, backupManifestName, tar.TypeCont, payload)))); err == nil {
		t.Fatal("accepted a directory as the manifest")
	}
	oversized := append(append([]byte(nil), payload...), bytes.Repeat([]byte(" "), portableManifestMaxSize+1-len(payload))...)
	if _, err := readPortableManifest(tar.NewReader(bytes.NewReader(tarWithNamedPayload(t, backupManifestName, tar.TypeReg, oversized)))); err == nil {
		t.Fatal("accepted an oversized manifest")
	}
}

func FuzzPortableManifestValidate(f *testing.F) {
	seed, err := json.Marshal(validPortableManifest())
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"version":4,"instance":{"kind":"orb","name":"x","created_at":"2026-01-01T00:00:00Z"},"files":{"rootfs":"rootfs.img"}}`))

	// Representative seeds for each checked field; the mutator explores the rest.
	seeds := []struct {
		name   string
		mutate func(*portableManifest)
	}{
		{"missing-name", func(m *portableManifest) { m.Instance.Name = "" }},
		{"wrong-rootfs", func(m *portableManifest) { m.Files.RootFS = "other.img" }},
		{"wrong-serial", func(m *portableManifest) { m.Files.SerialLog = "other.log" }},
		{"wrong-firecracker", func(m *portableManifest) { m.Files.FirecrackerLog = "other.log" }},
		{"missing-exported", func(m *portableManifest) { m.ExportedAt = time.Time{} }},
		{"unsupported", func(m *portableManifest) { m.Version = 5 }},
		{"pool-v2", func(m *portableManifest) { m.Instance.MemoryMode = model.MemoryModePool }},
		{"orb-v3", func(m *portableManifest) { m.Version = 3; m.Instance.Kind = model.KindOrb }},
	}
	for _, seed := range seeds {
		manifest := validPortableManifest()
		seed.mutate(&manifest)
		payload, err := json.Marshal(manifest)
		if err != nil {
			f.Fatalf("%s seed: %v", seed.name, err)
		}
		f.Add(payload)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		var manifest portableManifest
		if err := json.Unmarshal(data, &manifest); err != nil {
			return
		}
		if err := manifest.validate(); err != nil {
			return
		}
		// Independent model of the accepted contract: weakening validate must
		// not silently admit a manifest the importer cannot use.
		switch manifest.Version {
		case 1, portableManifestVersion, 3, 4:
		default:
			t.Fatalf("accepted version %d", manifest.Version)
		}
		if manifest.ExportedAt.IsZero() || manifest.Instance.CreatedAt.IsZero() {
			t.Fatalf("accepted manifest without timestamps: %+v", manifest)
		}
		if manifest.Instance.Name == "" {
			t.Fatalf("accepted manifest without a name: %+v", manifest)
		}
		if manifest.Files.RootFS != backupRootFSName {
			t.Fatalf("accepted rootfs entry %q", manifest.Files.RootFS)
		}
		if manifest.Files.SerialLog != "" && manifest.Files.SerialLog != backupSerialLogName {
			t.Fatalf("accepted serial log entry %q", manifest.Files.SerialLog)
		}
		if manifest.Files.FirecrackerLog != "" && manifest.Files.FirecrackerLog != backupFirecrackerName {
			t.Fatalf("accepted firecracker log entry %q", manifest.Files.FirecrackerLog)
		}
		if manifest.Instance.Kind != "" && manifest.Instance.Kind != model.KindVM && manifest.Instance.Kind != model.KindOrb {
			t.Fatalf("accepted kind %q", manifest.Instance.Kind)
		}
		if manifest.Instance.MemoryMode != "" && manifest.Instance.MemoryMode != model.MemoryModeFixed && manifest.Instance.MemoryMode != model.MemoryModePool {
			t.Fatalf("accepted memory mode %q", manifest.Instance.MemoryMode)
		}
		if manifest.Instance.Kind == model.KindOrb && manifest.Version < 4 {
			t.Fatalf("accepted orb manifest version %d", manifest.Version)
		}
		if manifest.Instance.MemoryMode == model.MemoryModePool && manifest.Version < 3 {
			t.Fatalf("accepted pooled manifest version %d", manifest.Version)
		}
		if manifest.Version == 1 && manifest.Instance.Image != "" && manifest.Instance.Image != "arch" {
			t.Fatalf("accepted non-Arch image %q in version 1", manifest.Instance.Image)
		}
	})
}

func FuzzReadPortableManifest(f *testing.F) {
	f.Add(tarWithNamedManifest(f, backupManifestName, validPortableManifest()))
	f.Add(tarWithNamedManifest(f, "other.json", validPortableManifest()))
	f.Add([]byte{})
	payload, err := json.Marshal(validPortableManifest())
	if err != nil {
		f.Fatal(err)
	}
	oversized := append(append([]byte(nil), payload...), bytes.Repeat([]byte(" "), portableManifestMaxSize+1-len(payload))...)
	f.Add(rawTarEntry(f, backupManifestName, tar.TypeCont, payload))
	f.Add(tarWithNamedPayload(f, backupManifestName, tar.TypeReg, oversized))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > portableManifestMaxSize+4096 {
			t.Skip()
		}
		manifest, err := readPortableManifest(tar.NewReader(bytes.NewReader(data)))
		if err != nil {
			return
		}
		// Envelope oracle: acceptance must agree with the first TAR header, not
		// only with the decoded manifest.
		header, err := tar.NewReader(bytes.NewReader(data)).Next()
		if err != nil {
			t.Fatalf("accepted stream has no first entry: %v", err)
		}
		if header.Name != backupManifestName {
			t.Fatalf("accepted manifest from entry %q", header.Name)
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != 0 {
			t.Fatalf("accepted non-regular entry type %d", header.Typeflag)
		}
		if header.Size <= 0 || header.Size > portableManifestMaxSize {
			t.Fatalf("accepted manifest size %d", header.Size)
		}
		if manifest.Instance.Name == "" || manifest.Files.RootFS != backupRootFSName || manifest.ExportedAt.IsZero() {
			t.Fatalf("accepted manifest lacks required fields: %+v", manifest)
		}
	})
}
