package nethelper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func networkPost(ctx context.Context, s *Server, path string, q any) *httptest.ResponseRecorder {
	data, _ := json.Marshal(q)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(data)).WithContext(ctx))
	return w
}

func TestNetworkFinalizationRetryAndLateRequests(t *testing.T) {
	withoutExistingRules(t)
	for _, failure := range []string{"mutation", "proof", "timeout"} {
		t.Run(failure, func(t *testing.T) {
			var mu sync.Mutex
			taps := map[string]bool{"tap-orb": true, "tap-neighbor": true}
			nat := map[string]bool{"eth0": true, "eth1": true}
			fail := true
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			runner := func(_ context.Context, name string, args ...string) error {
				mu.Lock()
				defer mu.Unlock()
				if name == "iptables" && args[1] == "nat" {
					if args[2] == "-D" {
						delete(nat, args[7])
					} else {
						nat[args[7]] = true
					}
				}
				if name == "ip" && args[0] == "tuntap" {
					if args[1] == "add" {
						taps[args[3]] = true
					}
					if args[1] == "del" {
						if fail && failure == "mutation" {
							return errors.New("busy")
						}
						if fail && failure == "timeout" {
							close(entered)
							<-release
							fail = false
						}
						delete(taps, args[3])
					}
				}
				return nil
			}
			output := func(_ context.Context, name string, _ ...string) ([]byte, error) {
				mu.Lock()
				defer mu.Unlock()
				if fail && failure == "proof" {
					return nil, errors.New("query failed")
				}
				if name == "ip" {
					rows := []map[string]string{}
					for tap := range taps {
						rows = append(rows, map[string]string{"ifname": tap})
					}
					return json.Marshal(rows)
				}
				if name == "nft" {
					return []byte(`{"nftables":[]}`), nil
				}
				if name == "iptables-save" && nat["eth0"] {
					return []byte("-s 10.0.0.0/30 -o eth0"), nil
				}
				return nil, nil
			}
			s := NewServerWithRunner(nil, "srv", runner)
			s.stateDir, s.output = t.TempDir(), output
			q := FinalizeRequest{RequestID: uuid.NewString(), CleanupRequest: CleanupRequest{InstanceID: uuid.NewString(), TapDevice: "tap-orb", NetworkCIDR: "10.0.0.0/30", OutboundInterface: "eth0"}}
			// Lifecycle admission accepts both forms, including in old journals.
			if failure == "mutation" {
				q.RequestID = "ABCDDCBA-1234-4234-8234-123456789ABC"
			} else if failure == "proof" {
				q.RequestID = uuid.Nil.String()
			}
			ctx := context.Background()
			if failure == "timeout" {
				first := make(chan *httptest.ResponseRecorder, 1)
				cancelCtx, cancel := context.WithCancel(ctx)
				defer cancel()
				go func() { first <- networkPost(cancelCtx, s, "/network/finalize-v1", q) }()
				<-entered
				cancel() // The old child intentionally ignores cancellation.
				waitCtx, stop := context.WithTimeout(ctx, 50*time.Millisecond)
				defer stop()
				if w := networkPost(waitCtx, s, "/network/finalize-v1", q); w.Code == http.StatusNoContent {
					t.Fatal("retry passed an undrained worker")
				}
				unblock()
				<-first // Reply may be lost regardless of its status.
			} else if w := networkPost(ctx, s, "/network/finalize-v1", q); w.Code == http.StatusNoContent {
				t.Fatal("injected failure reported success")
			}
			mu.Lock()
			fail = false
			mu.Unlock()
			// Restart the helper object; only its durable directory survives.
			restarted := NewServerWithRunner(nil, "srv", runner)
			restarted.stateDir, restarted.output = s.stateDir, output
			s = restarted
			q.OutboundInterface = "eth1" // The host's default route changed.
			if w := networkPost(ctx, s, "/network/finalize-v1", q); w.Code != http.StatusNoContent {
				t.Fatal(w.Body.String())
			}
			mu.Lock()
			gone, neighbor := !taps["tap-orb"] && !nat["eth0"], taps["tap-neighbor"] && nat["eth1"]
			mu.Unlock()
			if !gone || !neighbor {
				t.Fatal("wrong network removed")
			}
			// A new incarnation reuses the exact TAP/CIDR. Old requests must
			// neither recreate the predecessor nor destroy the successor.
			setup := SetupRequest{InstanceID: uuid.NewString(), TapDevice: q.TapDevice, HostAddr: "10.0.0.1/30", NetworkCIDR: q.NetworkCIDR, OutboundInterface: "eth0"}
			if w := networkPost(ctx, s, "/network/setup-v2", setup); w.Code != http.StatusNoContent {
				t.Fatal(w.Body.String())
			}
			q.OutboundInterface = "" // Completed retries need no route lookup.
			if w := networkPost(ctx, s, "/network/finalize-v1", q); w.Code != http.StatusNoContent {
				t.Fatal(w.Body.String())
			}
			if w := networkPost(ctx, s, "/network/cleanup-v2", q.CleanupRequest); w.Code == http.StatusNoContent {
				t.Fatal("late cleanup accepted")
			}
			setup.InstanceID = q.InstanceID
			if w := networkPost(ctx, s, "/network/setup-v2", setup); w.Code == http.StatusNoContent {
				t.Fatal("late setup accepted")
			}
			if failure == "mutation" {
				q.RequestID = strings.ToLower(q.RequestID)
			} else {
				q.RequestID = uuid.NewString()
			}
			if w := networkPost(ctx, s, "/network/finalize-v1", q); w.Code == http.StatusNoContent {
				t.Fatal("different request adopted deletion")
			}
			mu.Lock()
			successor, neighbor := taps["tap-orb"] && nat["eth0"], taps["tap-neighbor"] && nat["eth1"]
			mu.Unlock()
			if !successor || !neighbor {
				t.Fatal("late request destroyed reused or neighboring network")
			}
		})
	}
}

func TestNetworkChildRetainsLockAfterOwnerExits(t *testing.T) {
	s := NewServer(nil, "srv")
	if dir := os.Getenv("SRV_TEST_NETWORK_LOCK_OWNER"); dir != "" {
		s.stateDir = dir
		ctx, release, err := s.lockNetwork(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		if err := defaultRunner(ctx, "sh", "-c", `touch "$1"; while [ ! -e "$2" ]; do sleep 0.01; done`, "sh", filepath.Join(dir, "ready"), filepath.Join(dir, "finish")); err != nil {
			t.Fatal(err)
		}
		return
	}
	s.stateDir = t.TempDir()
	ready, finish := filepath.Join(s.stateDir, "ready"), filepath.Join(s.stateDir, "finish")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	owner := exec.Command(executable, "-test.run=^TestNetworkChildRetainsLockAfterOwnerExits$")
	owner.Env = append(os.Environ(), "SRV_TEST_NETWORK_LOCK_OWNER="+s.stateDir)
	owner.Stderr = os.Stderr
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(finish, nil, 0600)
		if owner.ProcessState == nil {
			_ = owner.Process.Kill()
			_ = owner.Wait()
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child did not start")
		}
		time.Sleep(time.Millisecond)
	}
	// Crash the actual owner process, leaving its mutating child alive.
	if err := owner.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := owner.Wait(); err == nil {
		t.Fatal("owner exited without being killed")
	}
	wait, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, unlock, err := s.lockNetwork(wait); err == nil {
		unlock()
		t.Fatal("lock released while mutating child survives")
	}
	if err := os.WriteFile(finish, nil, 0600); err != nil {
		t.Fatal(err)
	}
	wait, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if _, unlock, err := s.lockNetwork(wait); err != nil {
		t.Fatal(err)
	} else {
		unlock()
	}
}

func TestCleanupProofRejectsResidualsAndQueryFailures(t *testing.T) {
	q := CleanupRequest{TapDevice: "tap-proof", NetworkCIDR: "172.28.12.0/30"}
	for _, scenario := range []string{"clean", "neighbour", "tap", "route", "host-rule", "guest-rule", "query-failure", "invalid-json", "null", "object", "null-row", "missing-rules"} {
		t.Run(scenario, func(t *testing.T) {
			err := verifyNetworkAbsent(context.Background(), q, func(_ context.Context, name string, args ...string) ([]byte, error) {
				if scenario == "query-failure" {
					return nil, errors.New("permission denied")
				}
				if scenario == "invalid-json" {
					return []byte("invalid"), nil
				}
				if scenario == "null" {
					return []byte("null"), nil
				}
				if scenario == "object" {
					return []byte(`{}`), nil
				}
				if scenario == "null-row" {
					return []byte(`[null]`), nil
				}
				if name == "ip" {
					if scenario == "neighbour" {
						return []byte(`[{"dst":"172.28.12.20/30","dev":"tap-proof-neighbour"}]`), nil
					}
					if scenario == "tap" {
						return []byte(`[{"ifname":"tap-proof"}]`), nil
					}
					if scenario == "route" {
						return []byte(`[{"dst":"172.28.12.0/30"}]`), nil
					}
					return []byte(`[]`), nil
				}
				if name == "nft" {
					if scenario == "missing-rules" {
						return []byte(`{}`), nil
					}
					if scenario == "guest-rule" {
						return []byte(`{"nftables":[{"rule":{"addr":"172.28.12.2"}}]}`), nil
					}
					return []byte(`{"nftables":[]}`), nil
				}
				if scenario == "host-rule" {
					return []byte(`-A FORWARD -s 172.28.12.1/32 -j ACCEPT`), nil
				}
				return []byte(""), nil
			})
			if (err == nil) != (scenario == "clean" || scenario == "neighbour") {
				t.Fatal(err)
			}
		})
	}
}

func TestCleanupProofFindsIPv4InNATAndMappedAddresses(t *testing.T) {
	q := CleanupRequest{TapDevice: "tap-mine000001", NetworkCIDR: "172.28.1.4/30"}
	for _, tc := range []struct {
		rule     string
		residual bool
	}{
		{"--to-destination 172.28.1.6:22", true},
		{"--to-source 172.28.1.4-172.28.1.7", true},
		{"-d ::ffff:172.28.1.6", true},
		{"-d 172.28.1.6/32", true},
		{"-i tap-mine000001", true},
		{"-d 172.28.1.40/30 -i tap-mine000001-other", false},
		{"-d 1172.28.1.6", false},
		{"-d 172.28.1.6.9", false},
	} {
		t.Run(tc.rule, func(t *testing.T) {
			err := verifyNetworkAbsent(context.Background(), q, func(_ context.Context, name string, _ ...string) ([]byte, error) {
				switch name {
				case "ip":
					return []byte(`[]`), nil
				case "nft":
					return []byte(`{"nftables":[]}`), nil
				case "iptables-save":
					return []byte(tc.rule), nil
				default:
					return nil, nil
				}
			})
			if (err != nil) != tc.residual {
				t.Fatalf("residual=%v: %v", tc.residual, err)
			}
		})
	}
}
