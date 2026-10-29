package front_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"

	"srv/internal/coldstore"
	"srv/internal/front"
	"srv/internal/model"
	"srv/internal/vmrunner"
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
		for _, failure := range []string{"send-base", "receive-base", "send-disk", "receive-disk", "metadata", "import", "complete", "wrong-token", "success"} {
			t.Run(failure, func(t *testing.T) {
				backend := newMigrationBackend(t, policy, failure)
				r := router(backend.run)
				r.Transport = checkedTransport{t: t, next: r.Transport}
				out, stderr, err := run(r, append([]string{"migrate", "a/vm", "--to", "b"}, policy...)...)
				if failure == "success" {
					if err != nil || backend.count("b/start") != 1 || backend.count("a/start") != 0 || backend.count("a/cleanup") != 1 || !strings.Contains(out, "b/vm") {
						t.Fatalf("events=%v out=%s err=%v", backend.events, out, err)
					}
					return
				}
				wantComplete := 0
				if failure == "complete" {
					wantComplete = 1
				}
				if backend.count("a/complete") != wantComplete || backend.count("a/cleanup") != 0 || backend.count("a/prepare") != 1 {
					t.Fatalf("failure must not complete, clean up, or retry migration: events=%v", backend.events)
				}
				if failure == "wrong-token" && (err == nil || !strings.Contains(err.Error(), "receipt mismatch")) {
					t.Fatalf("stale receipt must not complete source: err=%v", err)
				}
				if failure != "wrong-token" {
					host := "a"
					if strings.HasPrefix(failure, "receive-") || failure == "import" {
						host = "b"
					}
					if err == nil || !strings.Contains(err.Error(), failure+" failed") || backend.count(host+"/"+failure) != 1 {
						t.Fatalf("failed action must be attempted once without retry: events=%v err=%v", backend.events, err)
					}
				}
				if err == nil || !strings.Contains(err.Error(), "token=") || !strings.Contains(err.Error(), "recover manually") || !strings.Contains(err.Error(), "do not retry automatically") || backend.count("a/start") != 0 || backend.count("b/start") != 0 || !strings.Contains(stderr, "prepare") {
					t.Fatalf("events=%v stderr=%s err=%v", backend.events, stderr, err)
				}
			})
		}
	}
}

// The fake retains shared bases across Router calls, but every disk and import
// is scoped to the newly prepared token. Stream producers and consumers run
// concurrently, so only the event log is shared between their goroutines.
type migrationBackend struct {
	t         *testing.T
	policy    []string
	failure   string
	mu        sync.Mutex
	events    []string
	tokens    map[string]string
	completed map[string]bool
	bases     map[string]bool
	disks     map[string]bool
	imported  map[string]bool
}

func newMigrationBackend(t *testing.T, policy []string, failure string) *migrationBackend {
	return &migrationBackend{t: t, policy: policy, failure: failure, tokens: map[string]string{}, completed: map[string]bool{}, bases: map[string]bool{}, disks: map[string]bool{}, imported: map[string]bool{}}
}

func (b *migrationBackend) count(event string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, got := range b.events {
		if got == event {
			n++
		}
	}
	return n
}

func (b *migrationBackend) run(h front.Host, args []string, in io.Reader, out io.Writer) error {
	cmd := args[0]
	if cmd == "--json" {
		cmd = args[1]
	}
	var q vmrunner.MigrationDiskRequest
	action := cmd
	if cmd == "migration-disk" {
		if len(args) < 2 {
			return errors.New("missing native migration request")
		}
		if args[1] == "import" {
			action = "import"
		} else {
			if len(args) != 2 {
				return fmt.Errorf("unexpected native migration args: %v", args)
			}
			data, err := base64.RawURLEncoding.DecodeString(args[1])
			if err != nil {
				return err
			}
			if err := json.Unmarshal(data, &q); err != nil {
				return err
			}
			action = q.Action
			if q.InstanceID != "source-id" || q.Name == "" || q.Token == "" {
				return fmt.Errorf("incomplete native request: %+v", q)
			}
			if action != "check" && q.Token != b.tokens[q.Name] {
				return errors.New("native request token differs from prepare")
			}
		}
	} else if cmd == "migration" {
		action = args[2]
	}
	event := h.Name + "/" + action
	if cmd == "migration-disk" && action == "prepare" {
		event = h.Name + "/disk-prepare"
	}
	b.mu.Lock()
	b.events = append(b.events, event)
	b.mu.Unlock()
	if action == b.failure || b.failure == "check-"+h.Name && action == "check" {
		return fmt.Errorf("%s failed", action)
	}
	switch cmd {
	case "inspect":
		if h.Name != "a" {
			return errors.New("inspect must run on source")
		}
		mode := "fixed"
		if len(b.policy) > 0 {
			mode = "pool"
		}
		_, err := fmt.Fprintf(out, `{"instance":{"id":"source-id","memory_mode":%q,"image":"arch","vcpu_count":1,"memory_mib":512,"rootfs_size_bytes":1000}}`, mode)
		return err
	case "import-placement":
		want := []string{"--json", "import-placement", args[2], "--image", "arch", "--cpus", "1", "--ram", "512MiB", "--rootfs-size", "1000B"}
		if len(b.policy) == 2 {
			want = append(want, b.policy...)
		}
		if h.Name != "b" || !reflect.DeepEqual(args, want) {
			return fmt.Errorf("preflight on %s: %v want %v", h.Name, args, want)
		}
		_, err := io.WriteString(out, `{"eligible":true}`)
		return err
	case "migration":
		name := args[3]
		switch action {
		case "prepare":
			if h.Name != "a" || args[4] != "b" || strings.Join(args[6:], " ") != strings.Join(b.policy, " ") {
				return fmt.Errorf("unexpected prepare: %s %v", h.Name, args)
			}
			if b.count("a/check") == 0 || b.count("b/check") == 0 {
				b.t.Error("source fenced before both hosts passed native capability checks")
			}
			b.tokens[name] = args[5]
			return json.NewEncoder(out).Encode(map[string]any{"instance_id": "source-id", "token": args[5], "target": args[4], "was_running": true, "phase": "prepared"})
		case "receipt":
			if h.Name != "b" || !b.imported[b.tokens[name]] {
				return errors.New("receipt requested before target import")
			}
			token := b.tokens[name]
			if b.failure == "wrong-token" {
				token = "stale-token"
			}
			return json.NewEncoder(out).Encode(map[string]any{"source_instance_id": "source-id", "phase": "imported", "token": token})
		case "complete":
			if h.Name != "a" || args[4] != b.tokens[name] || !b.imported[b.tokens[name]] || b.count("b/receipt") == 0 {
				return errors.New("source completion before verified target receipt")
			}
			b.completed[name] = true
			_, err := io.WriteString(out, `{}`)
			return err
		}
	case "migration-disk":
		wantHost := "a"
		if action == "base-present" || action == "receive-base" || action == "receive-disk" || action == "import" {
			wantHost = "b"
		}
		if action != "check" && h.Name != wantHost {
			return fmt.Errorf("%s on %s, want %s", action, h.Name, wantHost)
		}
		switch action {
		case "check":
			_, err := io.WriteString(out, `{}`)
			return err
		case "prepare":
			return json.NewEncoder(out).Encode(vmrunner.MigrationDiskResult{Transfer: vmrunner.MigrationDisk{
				Disk: coldstore.Object{ID: q.Token, Parent: "shared-base"}, Base: &coldstore.Object{ID: "shared-base"},
			}})
		case "base-present":
			if q.Transfer.Base == nil || q.Transfer.Disk.ID != q.Token || q.Transfer.Disk.Parent != q.Transfer.Base.ID {
				return errors.New("source transfer objects not forwarded to destination")
			}
			return json.NewEncoder(out).Encode(vmrunner.MigrationDiskResult{BasePresent: b.bases[q.Transfer.Base.ID]})
		case "send-base", "send-disk", "metadata":
			_, err := io.WriteString(out, q.Token)
			return err
		case "receive-base", "receive-disk":
			data, err := io.ReadAll(in)
			if err != nil {
				return err
			}
			if string(data) != q.Token || q.Transfer.Disk.ID != q.Token || q.Transfer.Base == nil {
				return errors.New("transfer stream or objects mismatch")
			}
			if action == "receive-base" {
				b.bases[q.Transfer.Base.ID] = true
			} else {
				if !b.bases[q.Transfer.Base.ID] {
					return errors.New("disk received before shared base")
				}
				b.disks[q.Token] = true
			}
			return nil
		case "import":
			if !reflect.DeepEqual(args, append([]string{"migration-disk", "import"}, b.policy...)) {
				return errors.New("import policy mismatch")
			}
			data, err := io.ReadAll(in)
			if err != nil {
				return err
			}
			token := string(data)
			if !b.disks[token] {
				return errors.New("metadata imported before disk transfer")
			}
			b.imported[token] = true
			return nil
		case "cleanup":
			if !b.completed[q.Name] {
				return errors.New("snapshot cleanup before source completion")
			}
			_, err := io.WriteString(out, `{}`)
			return err
		}
	case "start":
		if h.Name != "b" || !b.completed[args[1]] || b.count("a/cleanup") == 0 {
			return errors.New("start before source completion and snapshot cleanup")
		}
		return nil
	}
	return fmt.Errorf("unexpected command on %s: %v", h.Name, args)
}

func TestMigrationReusesSharedBase(t *testing.T) {
	backend := newMigrationBackend(t, nil, "")
	r := router(backend.run)
	for _, name := range []string{"vm-one", "vm-two"} {
		if out, stderr, err := run(r, "migrate", "a/"+name, "--to", "b"); err != nil || !strings.Contains(out, "b/"+name) {
			t.Fatalf("out=%s stderr=%s err=%v", out, stderr, err)
		}
	}
	for event, want := range map[string]int{"a/check": 2, "b/check": 2, "a/prepare": 2, "a/disk-prepare": 2, "b/base-present": 2, "a/send-base": 1, "b/receive-base": 1, "a/send-disk": 2, "b/receive-disk": 2, "a/metadata": 2, "b/import": 2, "a/complete": 2, "a/cleanup": 2, "b/start": 2} {
		if got := backend.count(event); got != want {
			t.Errorf("%s count=%d want=%d; events=%v", event, got, want, backend.events)
		}
	}
	if backend.tokens["vm-one"] == backend.tokens["vm-two"] {
		t.Error("distinct VM migrations reused a transfer token")
	}
}

func TestMigrationRequiresNativeCapabilityBeforePrepare(t *testing.T) {
	for _, host := range []string{"a", "b"} {
		t.Run(host, func(t *testing.T) {
			backend := newMigrationBackend(t, nil, "check-"+host)
			_, _, err := run(router(backend.run), "migrate", "a/vm", "--to", "b")
			if err == nil || !strings.Contains(err.Error(), "check failed") || backend.count(host+"/check") != 1 {
				t.Fatalf("missing capability must fail preflight: events=%v err=%v", backend.events, err)
			}
			for _, event := range []string{"a/prepare", "a/disk-prepare", "a/send-base", "a/send-disk", "b/import", "a/complete", "a/start", "b/start"} {
				if backend.count(event) != 0 {
					t.Errorf("capability failure must not mutate either host: events=%v", backend.events)
				}
			}
		})
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
		{[]string{"disk", "offload", "a/demo"}, []string{"disk", "offload", "demo"}},
		{[]string{"disk", "hydrate", "a/demo"}, []string{"disk", "hydrate", "demo"}},
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
		{"delete", "orb-name", "--expect", "request", "uuid", "created", "backend", "image"},
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
		if args[0] == "delete" || args[0] == "cleanup-receipt" || strings.HasPrefix(args[0], "operation-") {
			if _, _, err := run(r, args...); err == nil {
				t.Fatal("receipt command routed without host")
			}
		}
	}
	r := router(func(front.Host, []string, io.Reader, io.Writer) error {
		t.Fatal("removed delete-proof command reached a backend")
		return nil
	})
	if _, _, err := run(r, "--host", "b", "delete-proof", "request", "orb-name", "uuid", "created", "backend", "image"); err == nil {
		t.Fatal("removed delete-proof command accepted")
	}
}
