package front_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"

	"srv/internal/front"
	"srv/internal/model"
)

type fake func(front.Host, []string, io.Reader, io.Writer) error

type checkedTransport struct {
	t    *testing.T
	next front.Transport
}

func (c checkedTransport) Run(ctx context.Context, h front.Host, a model.Actor, args []string, in io.Reader, out, stderr io.Writer) error {
	if len(args) > 2 && args[0] == "--json" && args[1] == "migration" && (args[2] == "prepare" || args[2] == "complete") {
		if _, ok := ctx.Deadline(); ok {
			c.t.Error("migration mutation must not inherit read-only query timeout")
		}
	}
	return c.next.Run(ctx, h, a, args, in, out, stderr)
}

func (f fake) Run(_ context.Context, h front.Host, _ model.Actor, args []string, in io.Reader, out, stderr io.Writer) error {
	return f(h, args, in, out)
}
func router(f fake) *front.Router {
	return &front.Router{Hosts: []front.Host{{Name: "a"}, {Name: "b"}}, Transport: f}
}
func run(r *front.Router, args ...string) (string, string, error) {
	var out, stderr bytes.Buffer
	err := r.Run(context.Background(), model.Actor{}, args, nil, &out, &stderr)
	return out.String(), stderr.String(), err
}
func TestAggregatePartialAndUnsafeResolution(t *testing.T) {
	r := router(func(h front.Host, args []string, _ io.Reader, out io.Writer) error {
		if h.Name == "b" {
			return errors.New("offline")
		}
		io.WriteString(out, `{"instances":[{"name":"vm"}]}`)
		return nil
	})
	out, stderr, err := run(r, "--json", "list")
	if err == nil || !strings.Contains(out, `"host":"b"`) || !strings.Contains(stderr, "offline") {
		t.Fatalf("%s %s %v", out, stderr, err)
	}
	out, _, err = run(r, "list")
	if err == nil || !strings.Contains(out, "HOST") || !strings.Contains(out, "STATE") || !strings.Contains(out, "ERROR: ") {
		t.Fatalf("%s %v", out, err)
	}
	if _, _, err := run(r, "inspect", "vm"); err == nil || !strings.Contains(err.Error(), "cannot resolve") {
		t.Fatal(err)
	}
	r.Transport = fake(func(_ front.Host, args []string, _ io.Reader, out io.Writer) error {
		io.WriteString(out, `{"instances":[{"name":"vm"}]}`)
		return nil
	})
	if _, _, err := run(r, "inspect", "vm"); err == nil || !strings.Contains(err.Error(), "2 hosts") {
		t.Fatal(err)
	}
}
func TestPlacementEligibilityAndNoRetry(t *testing.T) {
	var mu sync.Mutex
	var creates []string
	r := router(func(h front.Host, args []string, _ io.Reader, out io.Writer) error {
		if args[0] == "--json" {
			if h.Name == "a" {
				io.WriteString(out, `{"eligible":false,"reason":"disk","memory_left_mib":999999,"disk_left_bytes":0}`)
			} else {
				io.WriteString(out, `{"eligible":true,"memory_left_mib":10,"disk_left_bytes":10}`)
			}
			return nil
		}
		mu.Lock()
		creates = append(creates, h.Name)
		mu.Unlock()
		return errors.New("connection lost after submission")
	})
	_, _, err := run(r, "new", "vm")
	if err == nil || !strings.Contains(err.Error(), "not retried") || len(creates) != 1 || creates[0] != "b" {
		t.Fatalf("creates=%v err=%v", creates, err)
	}
}
func TestMigrationCompletionOrdering(t *testing.T) {
	for _, policy := range [][]string{nil, {"--fixed-memory"}, {"--pool", "destination"}} {
		for _, failure := range []string{"export", "import", "complete", "wrong-token", "success"} {
			t.Run(failure, func(t *testing.T) {
				var mu sync.Mutex
				var starts []string
				completed := false
				var preparedToken string
				r := router(func(h front.Host, args []string, in io.Reader, out io.Writer) error {
					cmd := args[0]
					if cmd == "--json" {
						cmd = args[1]
					}
					switch cmd {
					case "inspect":
						mode := "fixed"
						if len(policy) > 0 {
							mode = "pool"
						}
						fmt.Fprintf(out, `{"instance":{"memory_mode":%q,"image":"arch","vcpu_count":1,"memory_mib":512,"rootfs_size_bytes":1000}}`, mode)
					case "import-placement":
						want := []string{"--json", "import-placement", "vm", "--image", "arch", "--cpus", "1", "--ram", "512MiB", "--rootfs-size", "1000B"}
						if len(policy) == 2 {
							want = append(want, policy...)
						}
						if !reflect.DeepEqual(args, want) {
							t.Errorf("preflight args %v want %v", args, want)
						}
						io.WriteString(out, `{"eligible":true}`)
					case "migration":
						if args[2] == "prepare" {
							if !reflect.DeepEqual(args[6:], policy) && !(len(args[6:]) == 0 && len(policy) == 0) {
								t.Errorf("prepare policy %v want %v", args[6:], policy)
							}
							preparedToken = args[5]
							return json.NewEncoder(out).Encode(map[string]any{"instance_id": "source-id", "token": args[5], "target": args[4], "was_running": true, "phase": "prepared"})
						}
						if args[2] == "receipt" {
							receiptToken := preparedToken
							if failure == "wrong-token" {
								receiptToken = "stale-token"
							}
							return json.NewEncoder(out).Encode(map[string]any{"source_instance_id": "source-id", "phase": "imported", "token": receiptToken})
						}
						if failure == "complete" {
							return errors.New("complete uncertain")
						}
						completed = true
						io.WriteString(out, `{}`)
					case "export":
						if failure == "export" {
							return errors.New("export failed")
						}
						_, err := io.WriteString(out, "archive")
						return err
					case "import":
						if !reflect.DeepEqual(args, append([]string{"import"}, policy...)) {
							return errors.New("import policy mismatch")
						}
						if failure == "import" {
							return errors.New("import failed")
						}
						_, err := io.Copy(io.Discard, in)
						return err
					case "start":
						if !completed {
							return errors.New("target start before source completion")
						}
						mu.Lock()
						starts = append(starts, h.Name)
						mu.Unlock()
					default:
						return errors.New("unexpected command")
					}
					return nil
				})
				r.Transport = checkedTransport{t: t, next: r.Transport}
				out, stderr, err := run(r, append([]string{"migrate", "a/vm", "--to", "b"}, policy...)...)
				if failure == "success" {
					if err != nil || len(starts) != 1 || starts[0] != "b" || !strings.Contains(out, "b/vm") {
						t.Fatalf("starts=%v out=%s err=%v", starts, out, err)
					}
					return
				}
				if failure == "wrong-token" && (completed || err == nil || !strings.Contains(err.Error(), "receipt mismatch")) {
					t.Fatalf("stale receipt must not complete source: completed=%v err=%v", completed, err)
				}
				if err == nil || !strings.Contains(err.Error(), "token=") || !strings.Contains(err.Error(), "recover manually") || len(starts) != 0 || !strings.Contains(stderr, "prepare") {
					t.Fatalf("starts=%v stderr=%s err=%v", starts, stderr, err)
				}
			})
		}
	}
}

func TestReservedIdentityWrapper(t *testing.T) {
	r := router(func(front.Host, []string, io.Reader, io.Writer) error {
		t.Fatal("must not contact backend")
		return nil
	})
	for _, args := range [][]string{{"forward", "fake"}, {"--json", "--host", "a", "forward", "fake"}} {
		if _, _, err := run(r, args...); err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Fatal(err)
		}
	}
}

func TestBackendArgumentContracts(t *testing.T) {
	for _, tc := range []struct{ args, want []string }{
		{[]string{"backup", "create", "a/demo"}, []string{"backup", "create", "demo"}},
		{[]string{"backup", "list", "a/demo"}, []string{"backup", "list", "demo"}},
		{[]string{"backup", "delete", "a/demo", "backup-id"}, []string{"backup", "delete", "demo", "backup-id"}},
		{[]string{"logs", "-f", "a/demo", "serial"}, []string{"logs", "-f", "demo", "serial"}},
		{[]string{"logs", "serial", "--follow", "a/demo"}, []string{"logs", "serial", "--follow", "demo"}},
		{[]string{"import", "--host", "a"}, []string{"import"}},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			var got []string
			r := router(func(h front.Host, args []string, _ io.Reader, _ io.Writer) error {
				if h.Name != "a" {
					t.Errorf("host=%s", h.Name)
				}
				got = append([]string(nil), args...)
				return nil
			})
			if _, _, err := run(r, tc.args...); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestNodeListShowsBudgetsAndUnavailableHosts(t *testing.T) {
	r := router(func(h front.Host, args []string, _ io.Reader, out io.Writer) error {
		if !reflect.DeepEqual(args, []string{"--json", "status"}) {
			return errors.New("node list must use authorized backend status")
		}
		if h.Name == "b" {
			return errors.New("offline")
		}
		_, err := io.WriteString(out, `{"hostname":"srv-a","cpu":{"load_1m":1.25},"instances":{"running":2,"total":5},"capacity":[{"resource":"cpu","allocated":6,"budget":8},{"resource":"memory","allocated":3221225472,"budget":8589934592},{"resource":"disk","allocated":10737418240,"budget":107374182400,"filesystem":{"available":75161927680}}]}`)
		return err
	})
	out, stderr, err := run(r, "node", "list")
	if err == nil || !strings.Contains(stderr, "offline") {
		t.Fatalf("partial node failure not reported: %s %v", stderr, err)
	}
	for _, want := range []string{"a", "b", "ERROR:", "6 / 8", "2 / 5", "1.25", "3.0 GiB / 8.0 GiB", "10.0 GiB / 100.0 GiB", "70.0 GiB"} {
		if !strings.Contains(out, want) {
			t.Fatalf("node overview missing %q:\n%s", want, out)
		}
	}
	jsonOut, _, err := run(r, "--json", "node", "list")
	var result struct {
		Hosts []front.Result `json:"hosts"`
	}
	if err == nil || json.Unmarshal([]byte(jsonOut), &result) != nil || len(result.Hosts) != 2 || result.Hosts[0].Error != "" || result.Hosts[1].Error == "" {
		t.Fatalf("invalid partial JSON result: %s %v", jsonOut, err)
	}
	if _, _, err := run(r, "node", "list", "--host", "a"); err != nil {
		t.Fatalf("explicit host should not depend on offline hosts: %v", err)
	}
}

func TestBoundOrbRoutesPreserveIdentityAndRequest(t *testing.T) {
	for _, args := range [][]string{
		{"start", "orb-name", "--expect", "request", "uuid", "created", "backend", "image"},
		{"stop", "orb-name", "--expect", "request", "uuid", "created", "backend", "image"},
		{"orb", "bootstrap", "orb-name", "--expect", "request", "uuid", "created", "backend", "image"},
		{"delete-proof", "request", "orb-name", "uuid", "created", "backend", "image"},
		{"cleanup-receipt", "request"},
		{"operation-result", "request"},
		{"operation-cancel", "request", "orb-name", "stop", "uuid", "created", "backend", "image"},
	} {
		calls := 0
		r := router(func(h front.Host, got []string, _ io.Reader, _ io.Writer) error {
			calls++
			if h.Name != "b" || !reflect.DeepEqual(got, args) {
				t.Fatalf("forwarded %s %q, want b %q", h.Name, got, args)
			}
			return nil
		})
		if _, _, err := run(r, append(append([]string{}, args...), "--host", "b")...); err != nil {
			t.Fatal(err)
		}
		if calls != 1 {
			t.Fatal("unexpected routing", calls)
		}
		if args[0] == "delete-proof" || args[0] == "cleanup-receipt" || strings.HasPrefix(args[0], "operation-") {
			if _, _, err := run(r, args...); err == nil {
				t.Fatal("receipt command routed without host")
			}
		}
	}
}
