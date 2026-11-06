package vmrunner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
)

// The subprocess VMM fixture also exposes Firecracker's format query. This
// deliberately works through /proc/PID/exe, without an injectable query stub.
func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == "--snapshot-version" {
		if os.Getenv("SRV_RAM_QUERY_FAIL") == "1" {
			os.Exit(2)
		}
		fmt.Println("v1.0.0")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// Exercise the runner RPC and real process ownership/stop, with only the
// Firecracker device API replaced by the subprocess fixture.
func TestRAMStateLifecycle(t *testing.T) {
	for _, mode := range []string{
		"resume", "discard", "changed-disk", "changed-config", "changed-binary-before-save",
		"compatible-update", "incompatible-format", "legacy-format", "legacy-changed-disk",
		"unfinished-source", "query-failure", "query-exit", "query-exec", "query-timeout",
		"save-query-failure", "restore-failure", "other-incarnation", "plain-stop", "dead-source", "cross-filesystem",
	} {
		t.Run(mode, func(t *testing.T) {
			s, _, _ := newRunnerShutdownTestServer(t)
			var logs bytes.Buffer
			s.log = slog.New(slog.NewTextHandler(&logs, nil))
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
				pid, done := startReceiptSocketProcess(t, s.config, q.Name,
					"SRV_RECEIPT_RAM_STAGE="+filepath.Join(jail.RootDir, "ram-save"),
					"SRV_RECEIPT_PAUSE_MARKER="+filepath.Join(root, "paused"))
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
				if err := os.WriteFile(s.config.FirecrackerBinary, []byte("#!/bin/sh\necho v1.0.1\n"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			stop := StopRequest{Name: q.Name, InstanceID: q.InstanceID, RequestID: q.RequestID, HostBootID: q.HostBootID, SaveState: mode != "plain-stop"}
			if mode == "save-query-failure" {
				t.Setenv("SRV_RAM_QUERY_FAIL", "1")
				if err := c.StopInstanceVM(ctx, stop); err == nil || !strings.Contains(err.Error(), "query Firecracker snapshot format") {
					t.Fatalf("producer query failure ignored: %v", err)
				}
				if _, err := os.Stat(filepath.Join(root, "paused")); !os.IsNotExist(err) {
					t.Fatalf("query failure paused the guest: %v", err)
				}
				if state, err := s.readRAMState(q.Name); err != nil || state != nil {
					t.Fatalf("query failure published RAM: %+v %v", state, err)
				}
				t.Setenv("SRV_RAM_QUERY_FAIL", "") // Retry must still save the running producer.
			}
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
			state, err := s.readRAMState(q.Name)
			if err != nil || state == nil || state.SnapshotVersion != "v1.0.0" {
				t.Fatalf("producer format: %+v %v", state, err)
			}
			f, err := os.Open(binary)
			if err != nil {
				t.Fatal(err)
			}
			digest, err := ramBinaryDigest(f)
			if err != nil || state.Binary != digest {
				t.Fatalf("producer digest: %q, want %q: %v", state.Binary, digest, err)
			}
			if mode == "compatible-update" || mode == "incompatible-format" || strings.HasPrefix(mode, "query-") || mode == "changed-config" || mode == "changed-disk" || mode == "unfinished-source" {
				s.config.FirecrackerBinary = filepath.Join(t.TempDir(), filepath.Base(binary))
				script := "#!/bin/sh\necho v1.0.1\n" // Even patch-level differences are incompatible.
				switch mode {
				case "compatible-update":
					script = "#!/bin/sh\necho v1.0.0\n"
				case "query-failure":
					script = "#!/bin/sh\necho 'unexpected output'\n"
				case "query-exit":
					script = "#!/bin/sh\nexit 1\n"
				case "query-exec":
					script = "not an executable"
				case "query-timeout":
					script = "#!/bin/sh\nexec sleep 30\n"
				}
				if err := os.WriteFile(s.config.FirecrackerBinary, []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "unfinished-source" {
				r, err := s.readReceipt(state.Attempt)
				if err != nil || r == nil {
					t.Fatalf("source receipt: %v %v", r, err)
				}
				r.SaveState = false
				if err := s.writeReceipt(*r); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "legacy-format" || mode == "legacy-changed-disk" {
				// Omit the field entirely, as in manifests from the first release.
				b, err := os.ReadFile(filepath.Join(stateDir, "state.json"))
				if err != nil {
					t.Fatal(err)
				}
				var legacy map[string]any
				if err := json.Unmarshal(b, &legacy); err != nil {
					t.Fatal(err)
				}
				delete(legacy, "SnapshotVersion")
				if err := atomicColdJSON(filepath.Join(stateDir, "state.json"), legacy, 0600); err != nil {
					t.Fatal(err)
				}
			}
			// A new helper must use the persisted image and receipts.
			restarted := NewServerWithHandlers(s.log, s.config, s.start, nil)
			launches := 0
			cold := mode == "discard" || mode == "incompatible-format" || mode == "legacy-format" || mode == "changed-binary-before-save"
			restarted.start = func(ctx context.Context, req StartRequest) (StartResponse, error) {
				launches++
				resume, err := restarted.checkRAMState(ctx, req)
				if err != nil {
					return StartResponse{}, err
				}
				if (resume == nil) != cold {
					t.Fatalf("resume=%v, expected cold=%v", resume, cold)
				}
				if mode == "restore-failure" {
					m, err := firecracker.NewMachine(ctx, firecracker.Config{})
					if err != nil {
						return StartResponse{}, err
					}
					m.Handlers.FcInit = m.Handlers.FcInit.Clear()
					if err := restarted.configureRAMRestore(m, req, jail); err != nil {
						t.Fatal(err)
					}
					// The real load handler fails on an unavailable API socket.
					err = m.Handlers.FcInit.Run(ctx, m)
					if err == nil {
						t.Fatal("restore unexpectedly succeeded")
					}
					return StartResponse{}, err
				}
				return s.start(ctx, req)
			}
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
			case "changed-disk", "legacy-changed-disk":
				if err := os.WriteFile(disk, []byte("different disk"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			resp, err = c.StartInstanceVM(ctx, q)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "changed-config" || mode == "changed-disk" || mode == "legacy-changed-disk" || mode == "other-incarnation" || mode == "unfinished-source" || strings.HasPrefix(mode, "query-") {
				reason := "--discard-state"
				if mode == "query-failure" {
					reason = "invalid Firecracker snapshot format"
				} else if strings.HasPrefix(mode, "query-") {
					reason = "query Firecracker snapshot format"
				} else if mode == "unfinished-source" {
					reason = "source has not finished stopping"
				}
				if resp.Outcome != StartAbsent || !strings.Contains(resp.Reason, reason) || launches != 0 {
					t.Fatalf("unsafe resume: %+v", resp)
				}
				if _, err := os.Stat(stateDir); err != nil {
					t.Fatalf("rejection lost saved RAM: %v", err)
				}
				return
			}
			if mode == "restore-failure" {
				if resp.Outcome != StartUnknown || !strings.Contains(resp.Reason, "quarantined") {
					t.Fatalf("restore failure not quarantined: %+v", resp)
				}
				resp, err = c.StartInstanceVM(ctx, q)
				if err != nil || resp.Outcome != StartUnknown || launches != 1 {
					t.Fatalf("retry bypassed fencing: %+v %v, launches=%d", resp, err, launches)
				}
				if _, err := os.Stat(filepath.Join(jail.RootDir, "ram-resume", "memory")); err != nil {
					t.Fatal("failed restore lost quarantined image:", err)
				}
				return
			}
			if resp.Outcome != StartRunning {
				t.Fatalf("resume admission: %+v", resp)
			}
			if cold {
				if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
					t.Fatalf("cold start retained RAM: %v", err)
				}
				b, err := os.ReadFile(disk)
				if err != nil || string(b) != "original guest disk" {
					t.Fatalf("cold start replaced disk: %q %v", b, err)
				}
				if mode != "discard" && !strings.Contains(logs.String(), "cold boot with existing disk") {
					t.Fatal("automatic discard has no diagnostic:", logs.String())
				}
				if err := c.StopInstanceVM(ctx, stop); err == nil {
					t.Fatal("closed receipt acknowledged discarded RAM")
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
