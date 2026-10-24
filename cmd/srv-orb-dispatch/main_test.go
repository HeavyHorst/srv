package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/anmitsu/go-shlex"
)

// Exercise the executable contract, including process restarts and SSH argv /
// stdin boundaries. The stand-in server retains VM disks across invocations.
func TestMain(m *testing.M) {
	switch os.Getenv("SRV_ORB_TEST_MODE") {
	case "dispatch":
		main()
		os.Exit(0)
	case "ssh":
		if err := fakeSSH(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// Keep the server's wire schema independent of the dispatcher's response types.
type testVM struct {
	ID         string `json:"id"`
	CreatedAt  string `json:"created_at"`
	BackendID  string `json:"backend_id"`
	Generation int64  `json:"bootstrap_generation"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Image      string `json:"image"`
	State      string `json:"state"`
	Proxy      string `json:"amp_connect_gateway"`
	Migration  string `json:"migration_phase"`
}

type testServer struct {
	Results       map[string]map[string]any
	Proof         map[string]any
	Creates       int
	LateStop      bool
	LoseBootstrap bool
	VM            *testVM
	Disk          string
	Templates     map[string]string
	LoseTemplate  bool
	Environment   string
	Host          string
	Unreachable   bool
	OtherDown     bool
	LoseCreate    bool
	RejectCreate  bool
	LoseStop      bool
	LoseDelete    bool
	BadInventory  bool
	FailGuest     bool
}

func fakeSSH() error {
	if os.Getenv("ORB_TOKEN") != "" || os.Getenv("AMP_ORB_PROVIDER_TOKEN") != "" {
		return fmt.Errorf("credential leaked into SSH environment")
	}
	path := os.Getenv("SRV_ORB_TEST_SERVER")
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var s testServer
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	write := func() error {
		b, err := json.Marshal(s)
		if err != nil {
			return err
		}
		return os.WriteFile(path, b, 0600)
	}
	destination, command := os.Args[len(os.Args)-2], os.Args[len(os.Args)-1]
	if destination != "srv" {
		return fmt.Errorf("dispatcher must not connect to guest SSH")
	}
	args, err := shlex.Split(command, true)
	if err != nil {
		return err
	}
	if len(args) >= 2 && args[0] == "orb" && args[1] == "bootstrap" {
		args = append([]string{"--json"}, args...)
	}
	if len(args) < 2 || args[0] != "--json" {
		return fmt.Errorf("expected --json")
	}
	if args[1] == "orb" {
		args = append(args[:1], args[2:]...)
	} else if args[1] == "new" || args[1] == "list" {
		return fmt.Errorf("dispatcher used ordinary VM inventory or creation")
	}
	host := ""
	for i, a := range args {
		if a == "--host" {
			host = args[i+1]
			args = args[:i]
			break
		}
	}
	if s.Unreachable || (s.OtherDown && host == "") {
		return fmt.Errorf("host unavailable")
	}
	if host != "" && host != s.Host {
		return fmt.Errorf("wrong host")
	}
	emit := func(v any) error { return json.NewEncoder(os.Stdout).Encode(v) }
	finish := func(operation string, generation int64) error {
		if len(args) != 9 || args[3] != "--expect" || s.VM == nil || args[5] != s.VM.ID || args[6] != s.VM.CreatedAt || args[7] != s.VM.BackendID || args[8] != s.VM.Image {
			return fmt.Errorf("unbound mutation")
		}
		if s.Results == nil {
			s.Results = map[string]map[string]any{}
		}
		if _, ok := s.Results[args[4]]; ok {
			return fmt.Errorf("duplicate request")
		}
		binding := append([]string{operation, s.VM.Name}, args[5:]...)
		if operation == "bootstrap" {
			binding = append(binding, strconv.FormatInt(generation, 10))
		}
		encoded, _ := json.Marshal(binding)
		s.Results[args[4]] = map[string]any{"id": args[4], "name": s.VM.Name, "command": operation, "arguments": string(encoded), "phase": "done", "exit_code": 0}
		return nil
	}
	switch args[1] {
	case "operation-result":
		result, ok := s.Results[args[2]]
		if !ok {
			return fmt.Errorf("request missing")
		}
		return emit(result)
	case "cleanup-receipt":
		if s.Proof == nil || s.Proof["request_id"] != args[2] {
			return fmt.Errorf("receipt missing")
		}
		return emit(s.Proof)
	case "bootstrap":
		var req struct {
			Token      string `json:"token"`
			AMPURL     string `json:"amp_url"`
			Proxy      string `json:"proxy"`
			Generation int64  `json:"generation"`
		}
		payload, err := io.ReadAll(os.Stdin)
		if err != nil || json.Unmarshal(payload, &req) != nil || req.Token == "" || s.VM == nil || args[2] != s.VM.Name || req.Generation <= s.VM.Generation {
			return fmt.Errorf("invalid bootstrap")
		}
		if strings.Contains(command, req.Token) {
			return fmt.Errorf("token leaked in argv")
		}
		s.Environment = fmt.Sprintf("ORB_TOKEN=%q\nAMP_URL=%q\nHTTPS_PROXY=%q", req.Token, req.AMPURL, req.Proxy)
		s.VM.Generation = req.Generation
		if s.FailGuest {
			if err := write(); err != nil {
				return err
			}
			return fmt.Errorf("guest accidentally printed %s", s.Environment)
		}
		if err := finish("bootstrap", req.Generation); err != nil {
			return err
		}
		if err := write(); err != nil {
			return err
		}
		if s.LoseBootstrap {
			return fmt.Errorf("lost bootstrap reply")
		}
	case "list":
		if s.BadInventory {
			fmt.Println(`{"hosts":[{"host":"b","data":{}}]}`)
			return nil
		}
		vs := []testVM{}
		if s.VM != nil {
			vs = append(vs, *s.VM)
		}
		data := map[string]any{"instances": vs}
		if s.Host == "" || host != "" {
			return emit(data)
		}
		return emit(map[string]any{"hosts": []any{
			map[string]any{"host": "a", "data": map[string]any{"instances": []testVM{}}},
			map[string]any{"host": s.Host, "data": data},
		}})
	case "new":
		if s.RejectCreate {
			return fmt.Errorf("create rejected")
		}
		if s.VM != nil {
			return fmt.Errorf("duplicate VM")
		}
		s.Creates++
		s.VM = &testVM{ID: fmt.Sprintf("12345678-1234-4234-8234-%012x", s.Creates), CreatedAt: "2026-01-02T03:04:05Z", BackendID: "backend-one", Name: args[2], Kind: "orb", Image: "amp-orb", State: "ready", Proxy: "http://10.0.0.1:8080"}
		s.Disk = "fresh disk"
		for i, arg := range args {
			if arg == "--template" {
				var ok bool
				s.Disk, ok = s.Templates[args[i+1]]
				if !ok {
					return fmt.Errorf("missing template")
				}
			}
		}
		if err := write(); err != nil {
			return err
		}
		if s.LoseCreate {
			return fmt.Errorf("connection lost after creation")
		}
	case "project-template":
		if (args[2] != "create" && args[2] != "verify") || s.VM == nil || s.VM.State != "stopped" || s.VM.Name != args[3] {
			return fmt.Errorf("template needs a stopped source")
		}
		if s.Templates == nil {
			s.Templates = map[string]string{}
		}
		if _, ok := s.Templates[args[4]]; !ok {
			if args[2] == "verify" {
				return fmt.Errorf("published template missing")
			}
			s.Templates[args[4]] = s.Disk
		}
		if err := write(); err != nil {
			return err
		}
		if s.LoseTemplate {
			return fmt.Errorf("connection lost after template publication")
		}
	case "inspect":
		return emit(map[string]any{"instance": s.VM})
	case "stop":
		if err := finish("stop", 0); err != nil {
			return err
		}
		if s.VM.State == "stopped" {
			return fmt.Errorf("already stopped")
		}
		s.VM.State = "stopped"
		if s.LateStop {
			s.Results[args[4]]["phase"] = "executing"
		}
		if err := write(); err != nil {
			return err
		}
		if s.LoseStop {
			return fmt.Errorf("connection lost after stop")
		}
	case "start":
		if err := finish("start", 0); err != nil {
			return err
		}
		if s.VM.State != "stopped" {
			return fmt.Errorf("already running")
		}
		s.VM.State = "ready"
	case "delete-proof":
		if len(args) != 8 || s.VM == nil || args[4] != s.VM.ID || args[5] != s.VM.CreatedAt || args[6] != s.VM.BackendID || args[7] != s.VM.Image {
			return fmt.Errorf("unbound delete")
		}
		s.Proof = map[string]any{"version": 3, "gateways_absent": true, "kind": "orb", "image": s.VM.Image, "backend_id": s.VM.BackendID, "request_id": args[2], "instance_id": s.VM.ID, "name": s.VM.Name, "created_at": s.VM.CreatedAt, "deleted_at": "2026-01-02T04:04:05Z", "auth_key_disposition": "never-minted", "retained_templates": []string{}, "ingress_closed": true, "runtime_absent": true, "jailer_absent": true, "cgroup_absent": true, "network_absent": true, "storage_absent": true, "backups_absent": true, "snapshots_absent": true}
		s.VM, s.Disk, s.Environment = nil, "", ""
		if err := write(); err != nil {
			return err
		}
		if s.LoseDelete {
			return fmt.Errorf("connection lost after deletion")
		}
	default:
		return fmt.Errorf("unexpected operation")
	}
	return write()
}

type harness struct {
	t       *testing.T
	dir     string
	program string
}

func newHarness(t *testing.T, host string) harness {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	h := harness{t, t.TempDir(), exe}
	if err := initialize(config{directory: filepath.Join(h.dir, "state"), endpoint: "srv"}); err != nil {
		t.Fatal(err)
	}
	h.write(testServer{Host: host})
	// ssh is the only external transport. Run our standalone server fixture.
	if err := os.WriteFile(filepath.Join(h.dir, "ssh"), []byte("#!/bin/sh\nexport SRV_ORB_TEST_MODE=ssh\nexec "+quote(exe)+" \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h harness) read() testServer {
	h.t.Helper()
	b, err := os.ReadFile(filepath.Join(h.dir, "server.json"))
	if err != nil {
		h.t.Fatal(err)
	}
	var s testServer
	if err := json.Unmarshal(b, &s); err != nil {
		h.t.Fatal(err)
	}
	return s
}

func (h harness) write(s testServer) {
	h.t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.dir, "server.json"), b, 0600); err != nil {
		h.t.Fatal(err)
	}
}

func (h harness) run(operation, token string, extra ...string) (string, error) {
	return h.runArgs(nil, operation, token, extra...)
}

func (h harness) runArgs(args []string, operation, token string, extra ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.program, args...)
	cmd.Env = append(os.Environ(),
		"PATH="+h.dir+":"+os.Getenv("PATH"), "SRV_ORB_TEST_MODE=dispatch",
		"SRV_ORB_TEST_SERVER="+filepath.Join(h.dir, "server.json"),
		"SRV_ORB_STATE_DIR="+filepath.Join(h.dir, "state"), "SRV_ORB_ENDPOINT=srv", "SRV_ORB_HOST=",
		"SRV_ORB_IMAGE=amp-orb", "ORB_IMAGE=amp-orb", "ORB_OPERATION="+operation,
		"ORB_SNAPSHOT_ID=",
		"ORB_ID=amp-thread/with spaces and 'quotes'", "ORB_TOKEN="+token,
		"AMP_ORB_PROVIDER_TOKEN=provider-secret", "AMP_URL=https://amp.example.test",
	)
	cmd.Env = append(cmd.Env, extra...)
	b, err := cmd.CombinedOutput()
	return string(b), err
}

func (h harness) must(operation, token string, extra ...string) {
	h.t.Helper()
	if out, err := h.run(operation, token, extra...); err != nil {
		h.t.Fatalf("%s failed: %v\n%s", operation, err, out)
	}
}

func TestDispatchLifecycle(t *testing.T) {
	for _, host := range []string{"", "b"} {
		t.Run("host="+host, func(t *testing.T) {
			h := newHarness(t, host)
			h.must("pause", "") // No VM yet.
			h.must("spawn", "first-token")
			s := h.read()
			name := s.VM.Name
			s.Disk = "uncommitted work survives pause"
			s.OtherDown = host != ""
			h.write(s)
			h.must("spawn", "second-token") // Must not start an already running VM.
			h.must("pause", "")
			h.must("pause", "")
			if h.read().VM.State != "stopped" {
				t.Fatal("pause did not stop VM")
			}
			// Changed placement config must not move an existing orb.
			h.must("spawn", "fresh-token", "SRV_ORB_HOST=other")
			s = h.read()
			if s.VM.Name != name || s.VM.State != "ready" || s.Disk != "uncommitted work survives pause" {
				t.Fatalf("resume replaced or failed to start the disk: %+v", s)
			}
			if !strings.Contains(s.Environment, `ORB_TOKEN="fresh-token"`) || !strings.Contains(s.Environment, `AMP_URL="https://amp.example.test"`) || !strings.Contains(s.Environment, `HTTPS_PROXY="http://10.0.0.1:8080"`) {
				t.Fatalf("missing fresh bootstrap configuration: %s", s.Environment)
			}
			files, _ := filepath.Glob(filepath.Join(h.dir, "state", "*.json"))
			for _, file := range files {
				b, _ := os.ReadFile(file)
				info, _ := os.Stat(file)
				if strings.Contains(string(b), "token") || info.Mode().Perm() != 0600 {
					t.Fatal("credentials or unsafe permissions in dispatch state")
				}
			}
			h.must("delete", "")
			h.must("delete", "")
			h.must("pause", "")
			if h.read().VM != nil || h.read().Disk != "" {
				t.Fatal("delete retained VM disk")
			}
			if _, err := h.run("spawn", "later-token"); err == nil {
				t.Fatal("deleted orb was resurrected")
			}
		})
	}
}

func TestDispatchUncertainOutcomes(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(fmt.Sprint(accepted), func(t *testing.T) {
			h := newHarness(t, "b")
			s := h.read()
			s.RejectCreate, s.LoseCreate = !accepted, accepted
			h.write(s)
			if _, err := h.run("spawn", "token", "SRV_ORB_HOST=b"); err == nil {
				t.Fatal("create failure not reported")
			}
			s = h.read()
			s.RejectCreate, s.LoseCreate = false, false
			h.write(s)
			if accepted {
				if _, err := h.run("spawn", "fresh-token"); err == nil {
					t.Fatal("unknown create adopted automatically")
				}
				cmd := exec.Command(h.program, "bind", "amp-thread/with spaces and 'quotes'", s.VM.ID)
				cmd.Env = append(os.Environ(), "PATH="+h.dir+":"+os.Getenv("PATH"), "SRV_ORB_TEST_MODE=dispatch", "SRV_ORB_TEST_SERVER="+filepath.Join(h.dir, "server.json"), "SRV_ORB_STATE_DIR="+filepath.Join(h.dir, "state"), "SRV_ORB_ENDPOINT=srv", "SRV_ORB_IMAGE=amp-orb")
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatal(err, string(out))
				}
				h.must("spawn", "fresh-token")
				s = h.read()
				s.Disk = "work saved before pause"
				s.LoseStop = true
				h.write(s)
				if _, err := h.run("pause", ""); err == nil {
					t.Fatal("lost stop response not reported")
				}
				h.must("pause", "") // Read terminal request, not stopped-state inference.
				s = h.read()
				if s.VM.State != "stopped" || s.Disk != "work saved before pause" {
					t.Fatal("pause retry did not preserve the stopped VM and its disk")
				}
				s.LoseDelete = true
				h.write(s)
				if _, err := h.run("delete", ""); err == nil {
					t.Fatal("lost delete response not reported")
				}
				h.must("delete", "") // Read committed receipt, not inventory absence.
			} else {
				if out, err := h.run("spawn", "token"); err == nil || !strings.Contains(out, "unknown") {
					t.Fatalf("uncertain create was retried: %v %s", err, out)
				}
				if h.read().VM != nil {
					t.Fatal("retry allocated another VM")
				}
			}
		})
	}
}

func TestObservedStoppedDoesNotResolveLateOperation(t *testing.T) {
	h := newHarness(t, "")
	h.must("spawn", "original")
	s := h.read()
	s.LateStop = true
	h.write(s)
	if _, err := h.run("pause", ""); err == nil {
		t.Fatal("unfinished operation succeeded")
	}
	s = h.read()
	if s.VM.State != "stopped" {
		t.Fatal("fixture must expose stopped before operation completes")
	}
	for _, operation := range []string{"spawn", "pause", "delete"} {
		if _, err := h.run(operation, "premature"); err == nil {
			t.Fatal("observation cleared pending", operation)
		}
	}
	if h.read().Environment != s.Environment {
		t.Fatal("fresh token delivered while old stop could still land")
	}
	// The old execution completes AFTER those observations. Its exact terminal
	// receipt, not another inspect, is the first admissible recovery evidence.
	for _, result := range s.Results {
		if result["command"] == "stop" {
			result["phase"] = "done"
		}
	}
	s.LateStop = false
	h.write(s)
	h.must("spawn", "after-terminal")
	if h.read().VM.State != "ready" || !strings.Contains(h.read().Environment, "after-terminal") {
		t.Fatal("terminal recovery failed")
	}
}

func TestBootstrapLostRepliesAndMonotonicRecovery(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(fmt.Sprint(terminal), func(t *testing.T) {
			h := newHarness(t, "")
			s := h.read()
			s.FailGuest, s.LoseBootstrap = !terminal, terminal
			h.write(s)
			if _, err := h.run("spawn", "old"); err == nil {
				t.Fatal("lost reply not reported")
			}
			s = h.read()
			if s.VM.Generation != 1 {
				t.Fatal("first generation not admitted")
			}
			s.FailGuest, s.LoseBootstrap = false, false
			h.write(s)
			h.must("spawn", "new")
			if s = h.read(); s.VM.Generation != 2 || !strings.Contains(s.Environment, `ORB_TOKEN="new"`) {
				t.Fatal("new generation not applied")
			}
			path := filepath.Join(h.dir, "state", s.VM.Name+".json")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var state map[string]any
			if err := json.Unmarshal(data, &state); err != nil {
				t.Fatal(err)
			}
			for _, bad := range []any{nil, float64(1)} {
				state["bootstrap_generation"] = bad
				if err := save(path, state); err != nil {
					t.Fatal(err)
				}
				if _, err := h.run("spawn", "rollback"); err == nil {
					t.Fatal("provider counter loss/rollback adopted")
				}
			}
			// Crash after fsync of generation 3 but before SSH: no srv row is
			// evidence of neither execution nor cancellation. Allocate 4, not 3.
			state["bootstrap_generation"], state["bootstrap_floor"] = 3, 2
			state["pending"], state["request_id"] = "bootstrap", "12345678-1111-4222-8333-123456789abc"
			if err := save(path, state); err != nil {
				t.Fatal(err)
			}
			h.must("spawn", "after-crash")
			s = h.read()
			if s.VM.Generation != 4 {
				t.Fatal("unsent generation reused", s.VM.Generation)
			}
			// Provider survives another process restart. Lower backend state is
			// not adopted, and higher backend state is not silently caught up.
			for _, bad := range []int64{0, 5} {
				s.VM.Generation = bad
				h.write(s)
				if _, err := h.run("spawn", "rollback"); err == nil {
					t.Fatal("rollback adopted", bad)
				}
			}
		})
	}
}

func TestDispatchRejectsUnsafeInputsAndInventory(t *testing.T) {
	for _, scenario := range []string{"unreachable", "bad-inventory", "wrong-image", "missing-token", "guest-error"} {
		t.Run(scenario, func(t *testing.T) {
			h := newHarness(t, "b")
			s := h.read()
			s.Unreachable = scenario == "unreachable"
			s.BadInventory = scenario == "bad-inventory"
			s.FailGuest = scenario == "guest-error"
			h.write(s)
			extra := []string{}
			if scenario == "wrong-image" {
				extra = append(extra, "ORB_IMAGE=unprepared")
			}
			if scenario == "missing-token" {
				extra = append(extra, "ORB_TOKEN=")
			}
			out, err := h.run("spawn", "private-token", extra...)
			if err == nil || strings.Contains(out, "private-token") {
				t.Fatalf("expected credential-safe failure: %v %s", err, out)
			}
			if scenario != "guest-error" && h.read().VM != nil {
				t.Fatal("invalid request allocated VM")
			}
		})
	}
}

func TestDispatchSerializesSameOrb(t *testing.T) {
	h := newHarness(t, "b")
	results := make(chan error, 2)
	for range 2 {
		go func() {
			out, err := h.run("spawn", "token")
			if err != nil {
				err = fmt.Errorf("%w: %s", err, out)
			}
			results <- err
		}()
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if h.read().VM == nil || h.read().Disk != "fresh disk" {
		t.Fatal("concurrent spawn did not leave one usable VM")
	}
}

func TestDispatchDoesNotReplaceMissingDisk(t *testing.T) {
	h := newHarness(t, "b")
	h.must("spawn", "token")
	s := h.read()
	s.VM, s.Disk = nil, ""
	h.write(s)
	if out, err := h.run("spawn", "token"); err == nil || !strings.Contains(out, "missing") {
		t.Fatalf("expected missing-disk error: %v %s", err, out)
	}
	if h.read().VM != nil {
		t.Fatal("lost orb was silently replaced with a fresh VM")
	}
}

func TestRetireCompute(t *testing.T) {
	for _, scenario := range []string{"running", "paused", "lost-reply", "pending-bootstrap"} {
		t.Run(scenario, func(t *testing.T) {
			host := "b"
			if scenario == "paused" {
				host = ""
			}
			h := newHarness(t, host)
			if scenario == "pending-bootstrap" {
				s := h.read()
				s.FailGuest = true
				h.write(s)
			}
			if out, err := h.run("spawn", "token", "SRV_ORB_HOST="+host); (err != nil) != (scenario == "pending-bootstrap") {
				t.Fatalf("unexpected bootstrap result: %v %s", err, out)
			}
			if scenario == "paused" {
				h.must("pause", "")
			}
			s := h.read()
			id, name := s.VM.ID, s.VM.Name
			s.Disk = "work deliberately discarded"
			s.OtherDown = host != "" // Retirement must stay on the recorded backend.
			s.LoseDelete = scenario == "lost-reply"
			h.write(s)
			args := []string{"retire", "amp-thread/with spaces and 'quotes'", id}
			out, err := h.runArgs(args, "spawn", "unrelated-token", "ORB_SNAPSHOT_ID=unrelated")
			if s.LoseDelete {
				if err == nil {
					t.Fatal("lost deletion reply reported success")
				}
				if _, err := h.run("spawn", "late"); err == nil {
					t.Fatal("pending retirement allowed spawn")
				}
				if _, err := h.runArgs([]string{"retire", args[1], "22222222-2222-4222-8222-222222222222"}, "", ""); err == nil {
					t.Fatal("pending retirement accepted wrong identity")
				}
				out, err = h.runArgs(args, "", "")
			}
			if err != nil || !strings.Contains(out, "compute retired") {
				t.Fatalf("retirement failed: %v %s", err, out)
			}
			if s := h.read(); s.VM != nil || s.Disk != "" || s.Environment != "" {
				t.Fatal("retirement left compute or disk behind")
			}
			proofPath := filepath.Join(h.dir, "state", name+".json.cleanup")
			before, err := os.ReadFile(proofPath)
			if err != nil {
				t.Fatal("no retained cleanup receipt", err)
			}
			var proof map[string]any
			if err := json.Unmarshal(before, &proof); err != nil || proof["instance_id"] != id || proof["version"] != float64(3) || proof["gateways_absent"] != true || proof["storage_absent"] != true {
				t.Fatal("retirement lacks bound v3 proof", err)
			}
			// Retrying the operator command and later Amp jobs cannot recreate
			// compute or overwrite the retained disposition.
			if out, err := h.runArgs(args, "", ""); err != nil {
				t.Fatal(err, out)
			}
			if _, err := h.runArgs([]string{"retire", args[1], "22222222-2222-4222-8222-222222222222"}, "", ""); err == nil {
				t.Fatal("completed retirement accepted wrong identity")
			}
			h.must("pause", "")
			if out, err := h.run("spawn", "later"); err == nil || !strings.Contains(out, "intentionally retired") || !strings.Contains(out, "fresh-start") {
				t.Fatalf("resume did not explain retirement: %v %s", err, out)
			}
			h.must("delete", "")
			after, err := os.ReadFile(proofPath)
			if err != nil || string(before) != string(after) || h.read().VM != nil {
				t.Fatal("retirement disposition changed", err)
			}
		})
	}
}

func TestRetireRejectsUnconfirmedCompute(t *testing.T) {
	for _, scenario := range []string{"wrong-id", "invalid-uuid", "malformed-command", "env-only", "unknown-orb", "missing-vm", "replaced-vm", "pending-stop", "deleted"} {
		t.Run(scenario, func(t *testing.T) {
			h := newHarness(t, "")
			h.must("spawn", "token")
			s := h.read()
			args := []string{"retire", "amp-thread/with spaces and 'quotes'", s.VM.ID}
			operation := "delete"
			switch scenario {
			case "wrong-id":
				args[2] = "22222222-2222-4222-8222-222222222222"
			case "invalid-uuid":
				args[2] = "not-a-uuid"
			case "malformed-command":
				args = args[:2]
			case "env-only":
				args, operation = nil, "retire"
			case "unknown-orb":
				args[1] = "unknown-orb"
			case "missing-vm":
				s.VM, s.Disk = nil, ""
				h.write(s)
			case "replaced-vm":
				s.VM.ID = "22222222-2222-4222-8222-222222222222"
				h.write(s)
			case "pending-stop":
				s.LateStop = true
				h.write(s)
				if _, err := h.run("pause", ""); err == nil {
					t.Fatal("expected pending stop")
				}
			case "deleted":
				h.must("delete", "")
			}
			before, err := os.ReadFile(filepath.Join(h.dir, "server.json"))
			if err != nil {
				t.Fatal(err)
			}
			// A malformed operator invocation must not fall back to an Amp
			// delete job inherited from the environment.
			if out, err := h.runArgs(args, operation, ""); err == nil {
				t.Fatal("unconfirmed retirement succeeded", out)
			}
			after, err := os.ReadFile(filepath.Join(h.dir, "server.json"))
			if err != nil || string(before) != string(after) {
				t.Fatal("rejected retirement changed server state", err)
			}
			if scenario == "unknown-orb" {
				// A typo must not leave a lock-only record that poisons a later
				// legitimate first spawn for that ID.
				h.must("delete", "")
				h.must("spawn", "new-token", "ORB_ID=unknown-orb")
			}
		})
	}
}

func TestFreshStartLifecycle(t *testing.T) {
	for _, template := range []bool{false, true} {
		t.Run(fmt.Sprint(template), func(t *testing.T) {
			h := newHarness(t, "b")
			h.must("spawn", "first-token", "SRV_ORB_HOST=b")
			statePath := filepath.Join(h.dir, "state", h.read().VM.Name+".json")
			var authorizations [][]string
			snapshot := "ORB_SNAPSHOT_ID="
			wantDisk := "fresh disk"
			if template {
				s := h.read()
				s.Disk = "prepared project"
				h.write(s)
				snapshot += "prepared-project"
				h.must("pause", "", snapshot)
				h.must("spawn", "template-token", snapshot)
				wantDisk = "prepared project"
			}
			for cycle := 0; cycle < 2; cycle++ {
				s := h.read()
				oldID, oldName := s.VM.ID, s.VM.Name
				s.Disk = "uncommitted work deliberately discarded"
				h.write(s)
				orbID := "amp-thread/with spaces and 'quotes'"
				if out, err := h.runArgs([]string{"retire", orbID, oldID}, "", ""); err != nil {
					t.Fatal(err, out)
				}
				retired, err := os.ReadFile(statePath)
				if err != nil {
					t.Fatal(err)
				}
				proofPath := filepath.Join(h.dir, "state", oldName+".json.cleanup")
				proof, err := os.ReadFile(proofPath)
				if err != nil {
					t.Fatal(err)
				}
				args := []string{"fresh-start", orbID, oldID}
				authorizations = append(authorizations, args)
				for i := 0; i < 2; i++ {
					if out, err := h.runArgs(args, "delete", "inherited", "ORB_SNAPSHOT_ID=unrelated"); err != nil {
						t.Fatal("fresh-start authorization failed", err, out)
					}
					if h.read().VM != nil {
						t.Fatal("authorization started compute without an Amp spawn")
					}
				}
				archive, err := os.ReadFile(statePath + ".retired-" + oldID)
				if err != nil || string(archive) != string(retired) {
					t.Fatal("prior identity/counter record was not retained exactly", err)
				}
				h.must("spawn", "fresh-token", snapshot)
				s = h.read()
				if s.VM.ID == oldID || s.VM.Name == oldName || s.VM.Generation != 1 || s.Disk != wantDisk || !strings.Contains(s.Environment, `ORB_TOKEN="fresh-token"`) {
					t.Fatalf("fresh spawn did not create an independent workspace: %+v", s.VM)
				}
				newID := s.VM.ID
				s.Disk = "new work survives resume"
				h.write(s)
				for _, old := range authorizations {
					if out, err := h.runArgs(old, "", ""); err != nil {
						t.Fatal("repeated authorization failed", err, out)
					}
				}
				h.must("pause", "")
				h.must("spawn", "resume-token")
				s = h.read()
				if s.VM.ID != newID || s.Disk != "new work survives resume" {
					t.Fatal("fresh workspace was reset instead of resumed")
				}
				retained, err := os.ReadFile(proofPath)
				if err != nil || string(retained) != string(proof) {
					t.Fatal("prior cleanup receipt was overwritten", err)
				}
			}
			id := h.read().VM.ID
			h.must("delete", "")
			for _, old := range authorizations {
				if out, err := h.runArgs(old, "", ""); err != nil || h.read().VM != nil {
					t.Fatal("historical authorization did not remain a no-op", err, out)
				}
			}
			if _, err := h.runArgs([]string{"fresh-start", "amp-thread/with spaces and 'quotes'", id}, "", ""); err == nil {
				t.Fatal("ordinary deletion allowed fresh start")
			}
		})
	}
}

func TestDeleteClosesRetirement(t *testing.T) {
	for _, lostReply := range []bool{false, true} {
		t.Run(fmt.Sprint(lostReply), func(t *testing.T) {
			h := newHarness(t, "")
			h.must("spawn", "token")
			s := h.read()
			id := s.VM.ID
			s.LoseDelete = lostReply
			h.write(s)
			args := []string{"retire", "amp-thread/with spaces and 'quotes'", id}
			if out, err := h.runArgs(args, "", ""); (err != nil) != lostReply {
				t.Fatal("unexpected retirement result", err, out)
			}
			h.must("delete", "")
			h.must("delete", "")
			args[0] = "fresh-start"
			if _, err := h.runArgs(args, "", ""); err == nil {
				t.Fatal("Amp deletion left fresh-start permission")
			}
			if _, err := h.run("spawn", "late"); err == nil || h.read().VM != nil {
				t.Fatal("Amp deletion allowed recreation")
			}
		})
	}
}

func TestFreshStartRequiresRetirementProof(t *testing.T) {
	for _, scenario := range []string{"managed", "pending-retirement", "wrong-id", "env-only", "malformed-command", "missing-proof", "partial-proof", "archive-conflict"} {
		t.Run(scenario, func(t *testing.T) {
			h := newHarness(t, "")
			h.must("spawn", "token")
			s := h.read()
			id, name := s.VM.ID, s.VM.Name
			path := filepath.Join(h.dir, "state", name+".json")
			args := []string{"retire", "amp-thread/with spaces and 'quotes'", id}
			if scenario != "managed" {
				s.LoseDelete = scenario == "pending-retirement"
				h.write(s)
				if out, err := h.runArgs(args, "", ""); (err != nil) != s.LoseDelete {
					t.Fatal(err, out)
				}
			}
			args[0] = "fresh-start"
			operation := "delete"
			switch scenario {
			case "wrong-id":
				args[2] = "22222222-2222-4222-8222-222222222222"
			case "env-only":
				args, operation = nil, "fresh-start"
			case "malformed-command":
				args = args[:2]
			case "missing-proof":
				if err := os.Remove(path + ".cleanup"); err != nil {
					t.Fatal(err)
				}
			case "partial-proof":
				s = h.read()
				delete(s.Proof, "gateways_absent")
				if err := save(path+".cleanup", s.Proof); err != nil {
					t.Fatal(err)
				}
			case "archive-conflict":
				if err := os.WriteFile(path+".retired-"+id, []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			server, err := os.ReadFile(filepath.Join(h.dir, "server.json"))
			if err != nil {
				t.Fatal(err)
			}
			if out, err := h.runArgs(args, operation, ""); err == nil {
				t.Fatal("unproven fresh start accepted", out)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(before) != string(after) {
				t.Fatal("rejection changed current incarnation", err)
			}
			after, err = os.ReadFile(filepath.Join(h.dir, "server.json"))
			if err != nil || string(server) != string(after) {
				t.Fatal("rejection changed backend state", err)
			}
		})
	}
}

func TestFreshStartDoesNotClaimOldSnapshots(t *testing.T) {
	for _, phase := range []string{"ready", "creating", "legacy-creating"} {
		t.Run(phase, func(t *testing.T) {
			h := newHarness(t, "")
			h.must("spawn", "token")
			id := h.read().VM.ID
			h.must("pause", "", "ORB_SNAPSHOT_ID=old-template")
			paths, err := filepath.Glob(filepath.Join(h.dir, "state", "project-*.json"))
			if err != nil || len(paths) != 1 {
				t.Fatal("expected one snapshot mapping", err)
			}
			data, err := os.ReadFile(paths[0])
			if err != nil {
				t.Fatal(err)
			}
			var mapping map[string]any
			if err := json.Unmarshal(data, &mapping); err != nil {
				t.Fatal(err)
			}
			if phase != "ready" {
				mapping["phase"] = "creating"
				if phase == "legacy-creating" {
					delete(mapping, "source_instance_id")
				}
				if err := save(paths[0], mapping); err != nil {
					t.Fatal(err)
				}
			}
			args := []string{"retire", "amp-thread/with spaces and 'quotes'", id}
			if out, err := h.runArgs(args, "", ""); err != nil {
				t.Fatal(err, out)
			}
			args[0] = "fresh-start"
			if out, err := h.runArgs(args, "", ""); err != nil {
				t.Fatal(err, out)
			}
			h.must("spawn", "new-token")
			s := h.read()
			s.Templates = nil // Never fill a missing old template from new work.
			s.Disk = "new workspace contents"
			h.write(s)
			if out, err := h.run("pause", "", "ORB_SNAPSHOT_ID=old-template"); err == nil || !strings.Contains(out, "source incarnation") {
				t.Fatal("replacement claimed old snapshot", err, out)
			}
			if len(h.read().Templates) != 0 {
				t.Fatal("old template recreated")
			}
			h.must("pause", "", "ORB_SNAPSHOT_ID=new-template")
			if len(h.read().Templates) != 1 {
				t.Fatal("replacement cannot publish its own snapshot")
			}
		})
	}
}

func TestProjectSnapshotLifecycle(t *testing.T) {
	for _, host := range []string{"", "b"} {
		t.Run("host="+host, func(t *testing.T) {
			h := newHarness(t, host)
			snapshot := "ORB_SNAPSHOT_ID=project/with spaces:revision"
			h.must("spawn", "initial", snapshot) // Unknown snapshot falls back to base.
			s := h.read()
			s.Disk = "repository and installed dependency"
			s.LoseTemplate = true
			h.write(s)
			if _, err := h.run("pause", "", snapshot); err == nil {
				t.Fatal("lost template response was not reported")
			}
			s = h.read()
			s.LoseTemplate = false
			h.write(s)
			h.must("pause", "", snapshot)
			h.must("spawn", "resumed", snapshot)
			s = h.read()
			s.Disk = "later thread work, not part of the template"
			h.write(s)
			h.must("pause", "", snapshot) // Retrying does not overwrite prepared template.
			s = h.read()
			saved := s.Templates
			s.Templates = nil
			h.write(s)
			if _, err := h.run("pause", "", snapshot); err == nil {
				t.Fatal("lost published template recreated from later source contents")
			}
			s = h.read()
			if len(s.Templates) != 0 {
				t.Fatal("missing template was replaced")
			}
			s.Templates = saved
			h.write(s)
			h.must("delete", "")
			s = h.read()
			s.OtherDown = host != ""
			h.write(s)
			h.must("spawn", "new-thread", snapshot, "ORB_ID=second-thread", "SRV_ORB_HOST=other")
			if s = h.read(); s.Disk != "repository and installed dependency" || s.VM.State != "ready" || !strings.Contains(s.Environment, `ORB_TOKEN="new-thread"`) {
				t.Fatalf("new thread did not reuse the prepared disk with fresh bootstrap: %+v", s)
			}
			if _, err := h.run("pause", "", snapshot, "ORB_ID=second-thread"); err == nil {
				t.Fatal("different orb overwrote immutable snapshot ID")
			}
		})
	}
}

func TestDeleteReceiptGatewayCompatibility(t *testing.T) {
	for _, scenario := range []string{"v2-pending", "v3-missing-proof", "v2-finalized"} {
		t.Run(scenario, func(t *testing.T) {
			h := newHarness(t, "")
			h.must("spawn", "synthetic")
			s := h.read()
			name := s.VM.Name
			s.LoseDelete = true
			h.write(s)
			if _, err := h.run("delete", ""); err == nil {
				t.Fatal("expected lost reply")
			}
			s = h.read()
			delete(s.Proof, "gateways_absent")
			if scenario != "v3-missing-proof" {
				s.Proof["version"] = 2
			}
			h.write(s)
			path := filepath.Join(h.dir, "state", name+".json")
			if scenario == "v2-finalized" {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var r record
				if err := json.Unmarshal(data, &r); err != nil {
					t.Fatal(err)
				}
				r.Phase = "deleted"
				if err := save(path, r); err != nil {
					t.Fatal(err)
				}
				if err := save(path+".cleanup", s.Proof); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			_, err = h.run("delete", "")
			if (err == nil) != (scenario == "v2-finalized") {
				t.Fatal("unexpected receipt acceptance", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(before) != string(after) {
				t.Fatal("historical/fenced state changed", err)
			}
			if scenario == "v2-finalized" {
				data, err := os.ReadFile(path + ".cleanup")
				if err != nil {
					t.Fatal(err)
				}
				var proof map[string]any
				if err := json.Unmarshal(data, &proof); err != nil {
					t.Fatal(err)
				}
				if proof["version"] != float64(2) || proof["gateways_absent"] != nil {
					t.Fatal("v2 proof upgraded", proof)
				}
				if _, err := h.run("spawn", "late"); err == nil {
					t.Fatal("historical deletion resurrected")
				}
			}
		})
	}
}
