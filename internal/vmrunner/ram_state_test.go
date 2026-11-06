package vmrunner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise the runner RPC and real process ownership/stop, with only the
// Firecracker device API replaced by the subprocess fixture.
func TestRAMStateLifecycle(t *testing.T) {
	for _, mode := range []string{"resume", "discard", "changed-disk", "changed-config", "changed-binary-before-save", "other-incarnation", "plain-stop", "dead-source", "cross-filesystem"} {
		t.Run(mode, func(t *testing.T) {
			s, _, _ := newRunnerShutdownTestServer(t)
			if mode == "cross-filesystem" {
				dir, err := os.MkdirTemp("/dev/shm", "srv-ram-")
				if err != nil {
					t.Skip("separate tmpfs unavailable:", err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(dir) })
				s.config.InstancesDir = dir
			}
			s.config.JailerUID, s.config.JailerGID = os.Getuid(), os.Getgid()
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			s.config.FirecrackerBinary = binary
			q := receiptRequest(t)
			root := filepath.Join(s.config.InstancesDir, q.Name)
			if err := os.MkdirAll(filepath.Join(root, "disk"), 0700); err != nil {
				t.Fatal(err)
			}
			disk := filepath.Join(root, "disk", "rootfs.img")
			if err := os.WriteFile(disk, []byte("original guest disk"), 0600); err != nil {
				t.Fatal(err)
			}
			jail, err := resolveJailerRuntimePaths(s.config.JailerBaseDir, binary, q.Name)
			if err != nil {
				t.Fatal(err)
			}
			var process *os.Process
			var exited <-chan struct{}
			s.start = func(context.Context, StartRequest) (StartResponse, error) {
				if err := os.MkdirAll(jail.RootDir, 0700); err != nil {
					return StartResponse{}, err
				}
				pid, done := startReceiptSocketProcess(t, s.config, q.Name, "SRV_RECEIPT_RAM_STAGE="+filepath.Join(jail.RootDir, "ram-save"))
				process, _ = os.FindProcess(pid)
				exited = done
				return StartResponse{PID: pid}, nil
			}
			c := receiptHTTPClient(t, s.Handler())
			ctx := context.Background()
			resp, err := c.StartInstanceVM(ctx, q)
			if err != nil || resp.Outcome != StartRunning {
				t.Fatalf("start: %+v %v", resp, err)
			}
			if mode == "dead-source" {
				if err := process.Kill(); err != nil {
					t.Fatal(err)
				}
				<-exited
			}
			if mode == "changed-binary-before-save" {
				s.config.FirecrackerBinary = filepath.Join(t.TempDir(), filepath.Base(binary))
				if err := os.WriteFile(s.config.FirecrackerBinary, []byte("new installed VMM, not the running executable"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			stop := StopRequest{Name: q.Name, InstanceID: q.InstanceID, RequestID: q.RequestID, HostBootID: q.HostBootID, SaveState: mode != "plain-stop"}
			err = c.StopInstanceVM(ctx, stop)
			if mode == "dead-source" {
				if err == nil || !strings.Contains(err.Error(), "no saved RAM") {
					t.Fatalf("dead source falsely saved: %v", err)
				}
				if err := c.StopInstanceVM(ctx, stop); err == nil {
					t.Fatal("retry falsely acknowledged a save that never happened")
				}
				return
			}
			if mode == "cross-filesystem" {
				if err == nil || !strings.Contains(err.Error(), "rename-compatible") {
					t.Fatalf("unsupported layout not rejected: %v", err)
				}
				// Rejection must precede durable save intent; ordinary stop still works.
				stop.SaveState = false
				if err := c.StopInstanceVM(ctx, stop); err != nil {
					t.Fatal("layout failure trapped normal stop:", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			<-exited
			stateDir := filepath.Join(root, "ram-state")
			if mode == "plain-stop" {
				if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
					t.Fatalf("ordinary stop saved RAM: %v", err)
				}
				return
			}
			for _, f := range []string{"memory", "vmstate"} {
				b, err := os.ReadFile(filepath.Join(stateDir, f))
				if err != nil || string(b) != "saved guest "+f {
					t.Fatalf("saved %s: %q %v", f, b, err)
				}
			}
			b, err := os.ReadFile(disk)
			if err != nil || string(b) != "original guest disk" {
				t.Fatalf("suspend ran guest shutdown: %q %v", b, err)
			}
			// Replaying save must not overwrite or regenerate the single image.
			if err := c.StopInstanceVM(ctx, stop); err != nil {
				t.Fatal(err)
			}
			// A new helper must use the persisted image and receipts.
			restarted := NewServerWithHandlers(nil, s.config, s.start, nil)
			c = receiptHTTPClient(t, restarted.Handler())
			q.RequestID = "bde61e32-fb19-420a-9f66-32575ba25959"
			q.Bootstrap = Bootstrap{Hostname: q.Name} // Enrollment data is not VM geometry.
			switch mode {
			case "discard":
				q.DiscardState = true
			case "changed-config":
				q.MemoryMiB++
			case "other-incarnation":
				q.InstanceID = "successor-instance"
			case "changed-disk":
				if err := os.WriteFile(disk, []byte("different disk"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			resp, err = c.StartInstanceVM(ctx, q)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "changed-config" || mode == "changed-disk" || mode == "other-incarnation" || mode == "changed-binary-before-save" {
				if resp.Outcome != StartAbsent || !strings.Contains(resp.Reason, "--discard-state") {
					t.Fatalf("unsafe resume: %+v", resp)
				}
				if _, err := os.Stat(stateDir); err != nil {
					t.Fatalf("rejection lost saved RAM: %v", err)
				}
				return
			}
			if resp.Outcome != StartRunning {
				t.Fatalf("resume admission: %+v", resp)
			}
			if mode == "discard" {
				if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
					t.Fatalf("cold start retained RAM: %v", err)
				}
			}
		})
	}
}

func TestRAMStateRejectsLegacySave(t *testing.T) {
	s, _, _ := newRunnerShutdownTestServer(t)
	c := receiptHTTPClient(t, s.Handler())
	if err := c.StopInstanceVM(context.Background(), StopRequest{Name: "demo", SaveState: true}); err == nil {
		t.Fatal("save without managed identity accepted")
	}
}
