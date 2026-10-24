package storage

import (
	"fmt"
	"strings"
	"testing"
)

func escapeMountInfoValue(value string) string {
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case ' ', '\t', '\n', '\\':
			fmt.Fprintf(&b, "\\%03o", value[i])
		default:
			b.WriteByte(value[i])
		}
	}
	return b.String()
}

func TestParseMountInfoLineDecodesEscapedFields(t *testing.T) {
	line := `36 35 98:0 / /mnt\040one rw,noatime master:1 - ext4 /dev/sda\040one rw`
	mount, err := parseMountInfoLine(line)
	if err != nil {
		t.Fatal(err)
	}
	if mount.MountPoint != "/mnt one" {
		t.Fatalf("MountPoint = %q, want %q", mount.MountPoint, "/mnt one")
	}
	if mount.Source != "/dev/sda one" {
		t.Fatalf("Source = %q, want %q", mount.Source, "/dev/sda one")
	}
	if mount.FSType != "ext4" {
		t.Fatalf("FSType = %q, want ext4", mount.FSType)
	}
}

func TestUnescapeMountInfoValue(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{`plain`, `plain`},
		{`\040`, ` `},
		{`\011`, "\t"},
		{`\012`, "\n"},
		{`\134`, `\`},
		{`a\134b`, `a\b`},
		{`\999`, `\999`},
		{`\04`, `\04`},
	} {
		if got := unescapeMountInfoValue(tc.in); got != tc.want {
			t.Errorf("unescapeMountInfoValue(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func FuzzParseMountInfoLine(f *testing.F) {
	f.Add("/mnt/one", "/dev/sda", "ext4")
	f.Add("/", "rootfs", "rootfs")
	f.Fuzz(func(t *testing.T, mountPoint, source, fsType string) {
		for _, field := range []string{mountPoint, source, fsType} {
			// Reject fields that would change the mountinfo field count or the
			// " - " separator; those are not parser inputs.
			if field == "-" || strings.TrimSpace(field) != field || len(strings.Fields(field)) != 1 || strings.Contains(field, `\`) {
				t.Skip()
			}
		}
		line := "36 35 98:0 / " + mountPoint + " rw - " + fsType + " " + source + " rw"
		mount, err := parseMountInfoLine(line)
		if err != nil {
			t.Fatalf("parse structured line: %v", err)
		}
		if mount.MountPoint != mountPoint || mount.Source != source || mount.FSType != fsType {
			t.Fatalf("parse = %+v, want mount=%q source=%q fstype=%q", mount, mountPoint, source, fsType)
		}
	})
}

func FuzzUnescapeMountInfoValue(f *testing.F) {
	f.Add(`plain`)
	f.Add(`\040`)
	f.Add(`a\134b`)
	f.Add(`\999`)
	f.Fuzz(func(t *testing.T, in string) {
		out := unescapeMountInfoValue(in)
		if len(out) > len(in) {
			t.Fatalf("unescape grew %q to %q", in, out)
		}
		if got := unescapeMountInfoValue(escapeMountInfoValue(in)); got != in {
			t.Fatalf("round trip %q -> %q -> %q", in, escapeMountInfoValue(in), got)
		}
	})
}
