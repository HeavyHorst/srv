package archbase

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const ownersScript = "overlay/usr/local/lib/srv/wazuh-owners.sh"

// runOwners runs wazuh-owners.sh against a scratch root holding conf as the
// package's tmpfiles.d entry. chown is recorded instead of run.
func runOwners(t *testing.T, conf string, setup func(root string)) (root, calls, out string, err error) {
	t.Helper()
	root, bin := t.TempDir(), t.TempDir()
	for _, name := range []string{"bash", "mkdir", "chmod"} {
		path, lookErr := exec.LookPath(name)
		if lookErr != nil {
			t.Skipf("%s unavailable: %v", name, lookErr)
		}
		if err := os.Symlink(path, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	callLog := filepath.Join(root, "calls")
	if err := os.WriteFile(filepath.Join(bin, "chown"), []byte("#!/bin/sh\necho chown \"$@\" >>"+callLog+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "usr/lib/tmpfiles.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "usr/lib/tmpfiles.d/wazuh-agent.conf"), []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "var/ossec/etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if setup != nil {
		setup(root)
	}
	script, absErr := filepath.Abs(ownersScript)
	if absErr != nil {
		t.Fatal(absErr)
	}
	cmd := exec.Command(filepath.Join(bin, "bash"), script)
	cmd.Env = []string{"PATH=" + bin, "SRV_WAZUH_ENROLL_ROOT=" + root}
	b, err := cmd.CombinedOutput()
	c, _ := os.ReadFile(callLog)
	return root, string(c), string(b), err
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

// The package's real entries put root-owned files in wazuh-owned
// directories, which systemd-tmpfiles refuses; the script must apply them.
func TestWazuhOwnersAppliesPackageEntries(t *testing.T) {
	conf := `d /var/ossec 0750 root wazuh -
d /var/ossec/etc 0770 wazuh wazuh -
z /var/ossec/etc/ossec.conf 0660 root wazuh -
d /var/ossec/etc/shared 0770 root wazuh -
z /var/ossec/etc/wpk_root.pem 0640 root wazuh -
d /var/ossec/logs 0770 wazuh wazuh -
f /var/ossec/logs/ossec.log 0660 wazuh wazuh -
d /var/ossec/tmp 1770 root wazuh -
`
	root, calls, out, err := runOwners(t, conf, func(root string) {
		os.WriteFile(filepath.Join(root, "var/ossec/etc/ossec.conf"), []byte("x"), 0o644)
	})
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	for path, want := range map[string]os.FileMode{
		"var/ossec":                0o750,
		"var/ossec/etc":            0o770,
		"var/ossec/etc/ossec.conf": 0o660,
		"var/ossec/etc/shared":     0o770,
		"var/ossec/logs/ossec.log": 0o660,
	} {
		if got := mode(t, filepath.Join(root, path)); got != want {
			t.Errorf("%s mode %o, want %o", path, got, want)
		}
	}
	if info, err := os.Lstat(filepath.Join(root, "var/ossec/tmp")); err != nil || info.Mode()&os.ModeSticky == 0 || info.Mode().Perm() != 0o770 {
		t.Errorf("var/ossec/tmp is not sticky 1770: %v, %v", info, err)
	}
	// A z entry for a file the package did not ship is skipped, not created.
	if _, err := os.Lstat(filepath.Join(root, "var/ossec/etc/wpk_root.pem")); err == nil {
		t.Error("z entry created a missing file")
	}
	for _, want := range []string{
		"chown -h -- root:wazuh " + root + "/var/ossec/etc/shared",
		"chown -h -- wazuh:wazuh " + root + "/var/ossec/logs/ossec.log",
	} {
		if !strings.Contains(calls, want+"\n") {
			t.Errorf("missing %q in:\n%s", want, calls)
		}
	}
}

func TestWazuhOwnersRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, conf, want string
		setup            func(root string)
	}{
		{name: "symlinked directory", conf: "d /var/ossec/etc/shared 0770 root wazuh -\n", want: "is a symlink", setup: func(root string) {
			os.Symlink("/etc", filepath.Join(root, "var/ossec/etc/shared"))
		}},
		{name: "symlinked parent", conf: "z /var/ossec/etc/x/passwd 0640 root wazuh -\n", want: "is a symlink", setup: func(root string) {
			os.Symlink("/etc", filepath.Join(root, "var/ossec/etc/x"))
		}},
		{name: "outside /var/ossec", conf: "z /etc/shadow 0644 root wazuh -\n", want: "refusing path"},
		{name: "dot-dot", conf: "z /var/ossec/../etc/shadow 0644 root wazuh -\n", want: "refusing path"},
		{name: "symlink type", conf: "L /var/ossec/etc/x 0644 root wazuh /etc/shadow\n", want: "unsupported entry type"},
		{name: "setuid mode", conf: "z /var/ossec/etc/ossec.conf 4755 root wazuh -\n", want: "invalid mode"},
		{name: "setgid mode", conf: "d /var/ossec/tmp 2770 root wazuh -\n", want: "invalid mode"},
		{name: "argument", conf: "f /var/ossec/etc/x 0644 root wazuh content\n", want: "unsupported argument"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, calls, out, err := runOwners(t, tc.conf, tc.setup)
			if err == nil || !strings.Contains(out, tc.want) {
				t.Fatalf("err %v, output %q, want %q", err, out, tc.want)
			}
			if calls != "" {
				t.Fatalf("changed owners before refusing: %s", calls)
			}
		})
	}
}
