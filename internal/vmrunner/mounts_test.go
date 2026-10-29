package vmrunner

import (
	"bufio"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestJailRejectsSymlinkSource(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	if err := os.WriteFile(source, []byte("disk"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(source, link); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target")
	if err := bindFileIntoJail(link, target, false); err == nil {
		t.Fatal("accepted symlink source")
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("created target for rejected source: %v", err)
	}
}

// Run explicitly as root in an isolated mount namespace:
// sudo unshare -m env SRV_TEST_JAIL_MOUNTS=1 <vmrunner.test> -test.run TestJailMountLifecycle
func TestJailMountLifecycle(t *testing.T) {
	if os.Getenv("SRV_TEST_JAIL_MOUNTS") != "1" {
		t.Skip("requires isolated privileged mount namespace")
	}
	dir := t.TempDir()
	jail := filepath.Join(dir, "jail")
	if err := os.Mkdir(jail, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := unmountJail(jail); err != nil {
			t.Error(err)
		}
	})
	source := filepath.Join(dir, "source")
	if err := os.WriteFile(source, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(jail, "disk")
	if err := bindFileIntoJail(source, target, false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("after"), 0o600); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(source); err != nil || string(data) != "after" {
		t.Fatalf("writes did not reach source: %q, %v", data, err)
	}
	readonly := filepath.Join(jail, "kernel")
	if err := bindFileIntoJail(source, readonly, true); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(readonly, []byte("corrupt"), 0o600); err == nil {
		t.Fatal("readonly artifact accepted write")
	}
	if err := cleanupJailedRuntimePaths(instanceRuntimePaths{SocketPath: filepath.Join(dir, "firecracker.sock")}, jailerRuntimePaths{WorkspaceDir: jail}); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(source); err != nil || string(data) != "after" {
		t.Fatalf("cleanup changed backing data: %q, %v", data, err)
	}
}

func TestDrainFromSeparateMountNamespace(t *testing.T) {
	if os.Getenv("SRV_TEST_JAIL_MOUNTS") != "1" {
		t.Skip("requires isolated privileged mount namespace")
	}
	s, _, cgroup := newRunnerShutdownTestServer(t)
	s.config.InstancesDir = shortReceiptDir(t)
	r := receiptRequest(t)
	jail, err := resolveJailerRuntimePaths(s.config.JailerBaseDir, s.config.FirecrackerBinary, r.Name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unmountJail(jail.WorkspaceDir) })
	client := receiptHTTPClient(t, s.Handler())
	// This process forks its namespace before the runner mounts anything,
	// reproducing systemd's independent ExecStart/ExecStop namespaces.
	child := exec.Command("unshare", "-m", "sh", "-c", `
echo ready
read proceed
if mountpoint -q "$1"; then echo unexpected-mount >&2; exit 1; fi
exec curl --fail --silent --show-error --unix-socket "$2" -X POST http://runner/vm/drain
`, "drain", filepath.Join(jail.RootDir, "disk"), client.socketPath)
	input, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	child.Stderr = &stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = input.Close() })
	if line, err := bufio.NewReader(output).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("child readiness: %q %v", line, err)
	}
	source := filepath.Join(t.TempDir(), "disk")
	if err := os.WriteFile(source, []byte("retained-data"), 0o600); err != nil {
		t.Fatal(err)
	}
	var pid int
	var exited <-chan struct{}
	s.start = func(context.Context, StartRequest) (StartResponse, error) {
		if err := os.MkdirAll(jail.RootDir, 0o755); err != nil {
			return StartResponse{}, err
		}
		if err := bindFileIntoJail(source, filepath.Join(jail.RootDir, "disk"), false); err != nil {
			return StartResponse{}, err
		}
		pid, exited = startReceiptSocketProcess(t, s.config, r.Name)
		writeRunningVMCgroup(t, cgroup, filepath.Join(firecrackerVMRootCgroupName, r.Name), pid)
		return StartResponse{PID: pid}, nil
	}
	oldRequest, oldRemove := requestGuestShutdown, removePath
	t.Cleanup(func() { requestGuestShutdown, removePath = oldRequest, oldRemove })
	requestGuestShutdown = func(ctx context.Context, _ string) error {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
			return err
		}
		select {
		case <-exited:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	removePath = os.RemoveAll // emulate cgroupfs removal of pseudo-files
	if resp, err := client.StartInstanceVM(context.Background(), r); err != nil || resp.Outcome != StartRunning {
		t.Fatalf("start: %+v %v", resp, err)
	}
	if _, err := io.WriteString(input, "drain\n"); err != nil {
		t.Fatal(err)
	}
	if err := child.Wait(); err != nil {
		t.Fatalf("remote drain: %v %s", err, &stderr)
	}
	if err := client.VerifyInstanceAbsent(context.Background(), resultRequest(r)); err != nil {
		t.Fatalf("drain did not close cleaned receipt: %v", err)
	}
	info, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil || strings.Contains(string(info), jail.RootDir+"/disk") {
		t.Fatalf("drain left owner namespace mount: %v", err)
	}
	if data, err := os.ReadFile(source); err != nil || string(data) != "retained-data" {
		t.Fatalf("drain modified disk: %q %v", data, err)
	}
	if _, err := client.StartInstanceVM(context.Background(), r); err == nil {
		t.Fatal("drained runner accepted a start")
	}
}
