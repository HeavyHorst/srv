package archbase

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	enrollScript = "overlay/usr/local/lib/srv/wazuh-enroll.sh"
	agentKey     = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

type enrollGuest struct {
	t     *testing.T
	root  string
	bin   string
	calls string
}

// newEnrollGuest lays out the paths wazuh-enroll.sh uses under a scratch root.
// chown and systemctl are recorded instead of run.
func newEnrollGuest(t *testing.T) *enrollGuest {
	t.Helper()
	g := &enrollGuest{t: t, root: t.TempDir(), bin: t.TempDir()}
	g.calls = filepath.Join(g.root, "calls")
	for _, name := range []string{"bash", "env", "jq", "base64", "flock", "stat", "id", "mktemp", "cat", "chmod", "mv", "sync", "sed", "grep", "wc", "rm", "sha256sum"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Skipf("%s unavailable: %v", name, err)
		}
		if err := os.Symlink(path, filepath.Join(g.bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"chown", "systemctl"} {
		stub := "#!/bin/sh\necho " + name + " \"$@\" >>" + g.calls + "\n"
		if err := os.WriteFile(filepath.Join(g.bin, name), []byte(stub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	conf, err := os.ReadFile("overlay/var/ossec/etc/ossec.conf")
	if err != nil {
		t.Fatal(err)
	}
	g.write("var/ossec/etc/ossec.conf", string(conf), 0o660)
	g.write("proc/sys/kernel/hostname", "vm-a\n", 0o644)
	for _, dir := range []string{"run/srv-wazuh", "var/lib/srv"} {
		if err := os.MkdirAll(filepath.Join(g.root, dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return g
}

func (g *enrollGuest) write(path, content string, mode os.FileMode) {
	g.t.Helper()
	path = filepath.Join(g.root, path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		g.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		g.t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		g.t.Fatal(err)
	}
}

func (g *enrollGuest) read(path string) string {
	g.t.Helper()
	b, err := os.ReadFile(filepath.Join(g.root, path))
	if err != nil {
		return ""
	}
	return string(b)
}

func (g *enrollGuest) run(args ...string) (string, error) {
	script, err := filepath.Abs(enrollScript)
	if err != nil {
		g.t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(g.bin, "bash"), append([]string{script}, args...)...)
	cmd.Env = []string{"PATH=" + g.bin, "SRV_WAZUH_ENROLL_ROOT=" + g.root}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func identity(id, name, ip, key, manager, port string) string {
	encoded := base64.StdEncoding.EncodeToString([]byte(id + " " + name + " " + ip + " " + key))
	return `{"version":1,"agent_id":"` + id + `","agent_name":"` + name + `","agent_key":"` + encoded +
		`","manager_address":"` + manager + `","manager_port":` + port + `}`
}

func TestWazuhEnrollInstallsApprovedIdentity(t *testing.T) {
	g := newEnrollGuest(t)
	if out, err := g.run("check"); err == nil {
		t.Fatalf("fresh image passed the startup check: %s", out)
	}
	g.write("run/srv-wazuh/enrollment.json", identity("012", "vm-a", "any", agentKey, "wazuh.example.ts.net", "1514"), 0o600)
	if out, err := g.run("enroll", filepath.Join(g.root, "run/srv-wazuh/enrollment.json")); err != nil {
		t.Fatalf("enroll: %v\n%s", err, out)
	}
	if got, want := g.read("var/ossec/etc/client.keys"), "012 vm-a any "+agentKey+"\n"; got != want {
		t.Fatalf("client.keys = %q, want %q", got, want)
	}
	info, err := os.Stat(filepath.Join(g.root, "var/ossec/etc/client.keys"))
	if err != nil || info.Mode().Perm() != 0o640 {
		t.Fatalf("client.keys mode = %v, %v", info.Mode(), err)
	}
	conf := g.read("var/ossec/etc/ossec.conf")
	for _, want := range []string{
		"      <address>wazuh.example.ts.net</address><!-- srv-managed -->\n",
		"      <port>1514</port><!-- srv-managed -->\n",
		"      <enabled>no</enabled>\n",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("ossec.conf lacks %q", want)
		}
	}
	if strings.Contains(conf, "SRV_WAZUH_MANAGER_ADDRESS") {
		t.Error("ossec.conf still has the placeholder")
	}
	if g.read("run/srv-wazuh/enrollment.json") != "" {
		t.Error("identity file left behind")
	}
	calls := g.read("calls")
	for _, want := range []string{"systemctl enable wazuh-agent.service\nsystemctl restart wazuh-agent.service\n", "chown root:wazuh "} {
		if !strings.Contains(calls, want) {
			t.Errorf("calls lack %q:\n%s", want, calls)
		}
	}
	if out, err := g.run("check"); err != nil {
		t.Fatalf("approved identity failed the startup check: %v\n%s", err, out)
	}

	// Re-enrollment replaces the previously rendered manager.
	g.write("run/srv-wazuh/enrollment.json", identity("013", "vm-a", "any", strings.ToUpper(agentKey), "100.64.0.9", "1515"), 0o600)
	if out, err := g.run("enroll", filepath.Join(g.root, "run/srv-wazuh/enrollment.json")); err != nil {
		t.Fatalf("re-enroll: %v\n%s", err, out)
	}
	conf = g.read("var/ossec/etc/ossec.conf")
	if !strings.Contains(conf, "<address>100.64.0.9</address><!-- srv-managed -->") ||
		!strings.Contains(conf, "<port>1515</port><!-- srv-managed -->") || strings.Contains(conf, "wazuh.example") {
		t.Fatalf("re-enrollment did not replace the manager:\n%s", conf)
	}

	// Keys not installed by srv-wazuh-enroll do not start the agent.
	g.write("var/ossec/etc/client.keys", "014 vm-a any "+agentKey+"\n", 0o640)
	if out, err := g.run("check"); err == nil || !strings.Contains(out, "does not match the approved identity") {
		t.Fatalf("tampered client.keys passed the startup check: %v\n%s", err, out)
	}
	g.write("var/ossec/etc/client.keys", "", 0o640)
	if _, err := g.run("check"); err == nil {
		t.Fatal("empty client.keys passed the startup check")
	}
}

func TestWazuhEnrollRejectsInvalidIdentity(t *testing.T) {
	valid := identity("012", "vm-a", "any", agentKey, "wazuh.example.ts.net", "1514")
	for _, tc := range []struct {
		name, content, want string
		mode                os.FileMode
		symlink             bool
	}{
		{name: "another guest", content: identity("012", "vm-b", "any", agentKey, "m", "1514"), want: "is not this guest"},
		{name: "key for another id", content: strings.Replace(identity("013", "vm-a", "any", agentKey, "m", "1514"), `"agent_id":"013"`, `"agent_id":"012"`, 1), want: "belongs to agent 013"},
		{name: "key for another name", content: strings.Replace(identity("012", "vm-b", "any", agentKey, "m", "1514"), `"agent_name":"vm-b"`, `"agent_name":"vm-a"`, 1), want: "belongs to agent 012 vm-b"},
		{name: "short key", content: identity("012", "vm-a", "any", agentKey[1:], "m", "1514"), want: "does not decode"},
		{name: "manager id", content: identity("000", "vm-a", "any", agentKey, "m", "1514"), want: "invalid agent_id"},
		{name: "xml in manager", content: identity("012", "vm-a", "any", agentKey, "m</address><x>", "1514"), want: "invalid manager_address"},
		{name: "sed delimiter in manager", content: identity("012", "vm-a", "any", agentKey, "m|e", "1514"), want: "invalid manager_address"},
		{name: "port zero", content: identity("012", "vm-a", "any", agentKey, "m", "0"), want: "invalid manager_port"},
		{name: "port too large", content: identity("012", "vm-a", "any", agentKey, "m", "65536"), want: "invalid manager_port"},
		{name: "port as string", content: identity("012", "vm-a", "any", agentKey, "m", `"1514"`), want: "not a version 1 identity"},
		{name: "newline in name", content: strings.Replace(valid, `"agent_name":"vm-a"`, `"agent_name":"vm-a\n013"`, 1), want: "not a version 1 identity"},
		{name: "other version", content: strings.Replace(valid, `"version":1`, `"version":2`, 1), want: "not a version 1 identity"},
		{name: "not json", content: "012 vm-a any " + agentKey, want: "not a version 1 identity"},
		{name: "readable by others", content: valid, mode: 0o644, want: "mode 0600"},
		{name: "symlink", content: valid, symlink: true, want: "no enrollment file"},
		{name: "oversized", content: valid + strings.Repeat(" ", 4096), want: "too large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newEnrollGuest(t)
			mode := tc.mode
			if mode == 0 {
				mode = 0o600
			}
			input := "run/srv-wazuh/enrollment.json"
			if tc.symlink {
				g.write("run/srv-wazuh/real.json", tc.content, mode)
				if err := os.Symlink(filepath.Join(g.root, "run/srv-wazuh/real.json"), filepath.Join(g.root, input)); err != nil {
					t.Fatal(err)
				}
			} else {
				g.write(input, tc.content, mode)
			}
			out, err := g.run("enroll", filepath.Join(g.root, input))
			if err == nil || !strings.Contains(out, tc.want) {
				t.Fatalf("enroll error = %v, want diagnostic %q:\n%s", err, tc.want, out)
			}
			if g.read("var/ossec/etc/client.keys") != "" || g.read("var/lib/srv/wazuh-enrolled") != "" {
				t.Fatal("rejected identity was installed")
			}
			if strings.Contains(g.read("calls"), "systemctl") {
				t.Fatal("rejected identity started the agent")
			}
			if !strings.Contains(g.read("var/ossec/etc/ossec.conf"), "SRV_WAZUH_MANAGER_ADDRESS") {
				t.Fatal("rejected identity changed ossec.conf")
			}
			if _, err := os.Lstat(filepath.Join(g.root, input)); !os.IsNotExist(err) {
				t.Fatal("rejected identity file left behind")
			}
		})
	}
}
