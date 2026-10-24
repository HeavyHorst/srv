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
}
