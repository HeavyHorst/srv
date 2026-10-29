package vmrunner

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestCleanupProofRequiresExactClosedAttemptAndAbsentResources(t *testing.T) {
	for _, scenario := range []string{"clean", "missing", "foreign", "open", "earlier-attempt", "jailer", "cgroup", "unpinned", "permission"} {
		t.Run(scenario, func(t *testing.T) {
			s, serviceRel, serviceCgroup := newRunnerShutdownTestServer(t)
			s.config.CgroupRoot = serviceRel
			s.config.InstancesDir = shortReceiptDir(t)
			q := resultRequest(receiptRequest(t))
			if scenario != "missing" {
				unlock, err := s.lockName(q.Name)
				if err != nil {
					t.Fatal(err)
				}
				// Closed journals retain old PID/Ticks; even PID reuse is not proof
				// of a live VM. The current runtime/cgroup checks decide absence.
				r := startReceipt{StartResultRequest: q, State: "closed", PID: os.Getpid(), Ticks: 1}
				if scenario == "foreign" {
					r.InstanceID = "different"
				}
				if scenario == "open" {
					r.State = "accepted"
					r.Digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
				}
				if err := s.writeReceipt(r); err != nil {
					unlock()
					t.Fatal(err)
				}
				if scenario == "earlier-attempt" {
					r.RequestID = "11111111-2222-4333-8444-555555555555"
					r.State = "accepted"
					r.Digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
					if err := s.writeReceipt(r); err != nil {
						unlock()
						t.Fatal(err)
					}
				}
				unlock()
			}
			if scenario == "jailer" {
				paths, err := resolveJailerRuntimePaths(s.config.JailerBaseDir, s.config.FirecrackerBinary, q.Name)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(paths.WorkspaceDir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "cgroup" {
				if err := os.MkdirAll(filepath.Join(serviceCgroup, firecrackerVMRootCgroupName, q.Name), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "unpinned" {
				s.config.CgroupRoot = ""
			}
			if scenario == "permission" {
				if os.Geteuid() == 0 {
					t.Skip("DAC test requires unprivileged caller")
				}
				if err := os.Chmod(s.config.InstancesDir, 0); err != nil {
					t.Fatal(err)
				}
				defer os.Chmod(s.config.InstancesDir, 0700)
			}
			err := VerifyCleanup(context.Background(), s.config, q)
			if (err == nil) != (scenario == "clean") {
				t.Fatal(err)
			}
		})
	}
}

func TestCleanupProofOldRunnerFailsClosed(t *testing.T) {
	client := receiptHTTPClient(t, http.NotFoundHandler())
	if err := client.VerifyInstanceAbsent(context.Background(), resultRequest(receiptRequest(t))); err == nil {
		t.Fatal("old runner 404 treated as absence")
	}
	if err := client.VerifyInstanceIdle(context.Background(), "demo"); err == nil {
		t.Fatal("old runner 404 treated as idle")
	}
}

func TestDeletionProofRequiresFenceAndAbsentRuntime(t *testing.T) {
	for _, scenario := range []string{"never-started", "closed", "missing-fence", "foreign-fence", "missing-attempt", "partial-attempt", "active", "jail", "cgroup", "socket"} {
		t.Run(scenario, func(t *testing.T) {
			s, root, cgroup := newRunnerShutdownTestServer(t)
			s.config.CgroupRoot = root
			q := resultRequest(receiptRequest(t))
			unlock, err := s.lockName(q.Name)
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "missing-fence" {
				id := q.InstanceID
				if scenario == "foreign-fence" {
					id = "another-incarnation"
				}
				if err := atomicColdJSON(s.coldDeletedPath(ColdRequest{Name: q.Name, InstanceID: q.InstanceID}), id, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "closed" || scenario == "active" {
				r := startReceipt{StartResultRequest: q, State: "closed"}
				if scenario == "active" {
					r.State, r.Digest = "accepted", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
				}
				if err := s.writeReceipt(r); err != nil {
					t.Fatal(err)
				}
			}
			unlock()
			if scenario != "closed" && scenario != "missing-attempt" && scenario != "partial-attempt" {
				q.RequestID, q.HostBootID = "", ""
			}
			if scenario == "partial-attempt" {
				q.HostBootID = ""
			}
			switch scenario {
			case "jail":
				paths, err := resolveJailerRuntimePaths(s.config.JailerBaseDir, s.config.FirecrackerBinary, q.Name)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(paths.WorkspaceDir, 0700); err != nil {
					t.Fatal(err)
				}
			case "cgroup":
				if err := os.MkdirAll(filepath.Join(cgroup, firecrackerVMRootCgroupName, q.Name), 0700); err != nil {
					t.Fatal(err)
				}
			case "socket":
				receiptRuntime(t, s.config, q.Name)
			}
			client := receiptHTTPClient(t, s.Handler())
			err = client.VerifyInstanceAbsent(context.Background(), q)
			if (err == nil) != (scenario == "never-started" || scenario == "closed") {
				t.Fatalf("deletion proof: %v", err)
			}
		})
	}
}

func TestStorageIdleRequiresVacantName(t *testing.T) {
	for _, scenario := range []string{"never-started", "active-receipt", "jail", "cgroup", "live-socket"} {
		t.Run(scenario, func(t *testing.T) {
			s, serviceRel, serviceCgroup := newRunnerShutdownTestServer(t)
			s.config.CgroupRoot = serviceRel
			s.config.InstancesDir = shortReceiptDir(t)
			switch scenario {
			case "active-receipt":
				unlock, err := s.lockName("demo")
				if err != nil {
					t.Fatal(err)
				}
				err = s.writeReceipt(startReceipt{StartResultRequest: resultRequest(receiptRequest(t)), State: "accepted", Digest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"})
				unlock()
				if err != nil {
					t.Fatal(err)
				}
			case "jail":
				paths, err := resolveJailerRuntimePaths(s.config.JailerBaseDir, s.config.FirecrackerBinary, "demo")
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(paths.WorkspaceDir, 0o700); err != nil {
					t.Fatal(err)
				}
			case "cgroup":
				if err := os.MkdirAll(filepath.Join(serviceCgroup, firecrackerVMRootCgroupName, "demo"), 0o700); err != nil {
					t.Fatal(err)
				}
			case "live-socket":
				receiptRuntime(t, s.config, "demo")
			}
			client := receiptHTTPClient(t, s.Handler())
			err := client.VerifyInstanceIdle(context.Background(), "demo")
			if (err == nil) != (scenario == "never-started") {
				t.Fatalf("idle result: %v", err)
			}
		})
	}
}
