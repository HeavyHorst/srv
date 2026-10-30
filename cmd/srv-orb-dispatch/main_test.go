package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/anmitsu/go-shlex"
	"golang.org/x/sys/unix"
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
	Results         map[string]map[string]any
	Proof           map[string]any
	ProofVersion    int
	ProxyBypass     *string
	Creates         int
	CreateAttempts  int
	LateStop        bool
	LoseBootstrap   bool
	VM              *testVM
	Disk            string
	Templates       map[string]string
	LoseTemplate    bool
	Environment     string
	Host            string
	Unreachable     bool
	OtherDown       bool
	LoseCreate      bool
	RejectCreate    bool
	RejectStart     bool
	LoseStop        bool
	LoseDelete      bool
	RejectDelete    bool
	DeleteRequestID string
	BadInventory    bool
	FailGuest       bool
}

func fakeSSH() error {
	if os.Getenv("ORB_TOKEN") != "" || os.Getenv("AMP_ORB_PROVIDER_TOKEN") != "" {
		return fmt.Errorf("credential leaked into SSH environment")
	}
	if _, ok := os.LookupEnv("SRV_ORB_PROXY_BYPASS"); ok {
		return fmt.Errorf("bypass configuration leaked into SSH environment")
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
		key, value := "request_id", args[2]
		if len(args) == 4 && args[2] == "--instance" {
			key, value = "instance_id", args[3]
		}
		if s.Proof == nil || s.Proof[key] != value {
			return fmt.Errorf("receipt missing")
		}
		return emit(s.Proof)
	case "bootstrap":
		var req struct {
			Token       string `json:"token"`
			AMPURL      string `json:"amp_url"`
			Proxy       string `json:"proxy"`
			ProxyBypass string `json:"proxy_bypass"`
			Generation  int64  `json:"generation"`
		}
		var keys map[string]any
		payload, err := io.ReadAll(os.Stdin)
		decoder := json.NewDecoder(bytes.NewReader(payload))
		decoder.DisallowUnknownFields()
		if err != nil || decoder.Decode(&req) != nil || json.Unmarshal(payload, &keys) != nil || req.Token == "" || s.VM == nil || args[2] != s.VM.Name || req.Generation <= s.VM.Generation {
			return fmt.Errorf("invalid bootstrap")
		}
		if value, ok := keys["proxy_bypass"]; ok {
			text, _ := value.(string)
			s.ProxyBypass = &text
		} else {
			s.ProxyBypass = nil
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
		s.CreateAttempts++
		if err := write(); err != nil {
			return err
		}
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
		if s.RejectStart {
			s.Results[args[4]]["phase"], s.Results[args[4]]["exit_code"] = "rejected", 1
			if err := write(); err != nil {
				return err
			}
			return fmt.Errorf("insufficient host memory")
		}
		if s.VM.State != "stopped" {
			return fmt.Errorf("already running")
		}
		s.VM.State = "ready"
	case "delete":
		if len(args) == 3 && s.VM != nil && args[2] == s.VM.Name {
			// Model ordinary SSH deletion independently of provider state.
			args = append(args, "--expect", fmt.Sprintf("abcdefab-1234-4234-8234-%012x", s.Creates), s.VM.ID, s.VM.CreatedAt, s.VM.BackendID, s.VM.Image)
		}
		if len(args) != 9 || args[3] != "--expect" || s.VM == nil || args[2] != s.VM.Name || args[5] != s.VM.ID || args[6] != s.VM.CreatedAt || args[7] != s.VM.BackendID || args[8] != s.VM.Image {
			return fmt.Errorf("unbound delete")
		}
		if s.DeleteRequestID != "" && s.DeleteRequestID != args[4] {
			return fmt.Errorf("deletion request changed on retry")
		}
		s.DeleteRequestID = args[4]
		if s.RejectDelete {
			if err := write(); err != nil {
				return err
			}
			return fmt.Errorf("cleanup preflight failed")
		}
		s.Proof = map[string]any{"version": 3, "gateways_absent": true, "kind": "orb", "image": s.VM.Image, "backend_id": s.VM.BackendID, "request_id": args[4], "instance_id": s.VM.ID, "name": s.VM.Name, "created_at": s.VM.CreatedAt, "deleted_at": "2026-01-02T04:04:05Z", "auth_key_disposition": "never-minted", "retained_templates": []string{}, "ingress_closed": true, "runtime_absent": true, "jailer_absent": true, "cgroup_absent": true, "network_absent": true, "storage_absent": true, "backups_absent": true, "snapshots_absent": true}
		if s.ProofVersion >= 4 {
			s.Proof["version"], s.Proof["snapshots_absent"], s.Proof["retained_templates"] = s.ProofVersion, false, nil
		}
		if s.ProofVersion == 5 {
			s.Proof["backups_absent"] = false
		}
		s.VM, s.Disk, s.Environment = nil, "", ""
		s.DeleteRequestID = ""
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

func (h harness) ssh(args ...string) (string, error) {
	args = append([]string{"--json"}, args...)
	if host := h.read().Host; host != "" {
		args = append(args, "--host", host)
	}
	for i := range args {
		args[i] = quote(args[i])
	}
	cmd := exec.Command(h.program, "srv", strings.Join(args, " "))
	cmd.Env = []string{"SRV_ORB_TEST_MODE=ssh", "SRV_ORB_TEST_SERVER=" + filepath.Join(h.dir, "server.json")}
	b, err := cmd.CombinedOutput()
	return string(b), err
}

func TestDispatchRetriesRejectedStart(t *testing.T) {
	for _, phase := range []string{"rejected", "failed", "executing", "interrupted", "wrong-binding"} {
		t.Run(phase, func(t *testing.T) {
			h := newHarness(t, "")
			h.must("spawn", "initial-token")
			h.must("pause", "")
			s := h.read()
			s.RejectStart, s.Disk = true, "saved workspace"
			h.write(s)
			if _, err := h.run("spawn", "rejected-token"); err == nil {
				t.Fatal("rejected start reported success")
			}
			s = h.read()
			var rejectedID string
			for id, result := range s.Results {
				if result["command"] == "start" {
					rejectedID = id
					if phase == "wrong-binding" {
						result["arguments"] = `[]`
					} else {
						result["phase"] = phase
					}
				}
			}
			if rejectedID == "" {
				t.Fatal("missing start request")
			}
			s.RejectStart = false
			h.write(s)
			_, err := h.run("spawn", "retry-token")
			if (err == nil) != (phase == "rejected") {
				t.Fatal("incorrect retry disposition", err)
			}
			s = h.read()
			starts := 0
			for id, result := range s.Results {
				if result["command"] == "start" {
					starts++
					if id != rejectedID && result["phase"] != "done" {
						t.Fatal("new start did not finish")
					}
				}
			}
			wantStarts, wantState := 1, "stopped"
			if phase == "rejected" {
				wantStarts, wantState = 2, "ready"
			}
			if starts != wantStarts || s.VM.State != wantState || s.Creates != 1 || s.Disk != "saved workspace" {
				t.Fatal("retry replayed or replaced the workspace", starts, s.VM)
			}
		})
	}
}

func TestOrbNamesFitDefaultSocketPaths(t *testing.T) {
	h := newHarness(t, "")
	dir, err := os.MkdirTemp("", "sock-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	// Match the default jailer prefix length, but bind in disposable storage.
	prefix := filepath.Join(dir, strings.Repeat("j", len("/var/lib/srv/jailer/firecracker")-len(dir)-1))
	for cycle := 0; cycle < 2; cycle++ {
		h.must("spawn", "token")
		path := filepath.Join(prefix, h.read().VM.Name, "root", "firecracker.sock")
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		listener, err := net.Listen("unix", path)
		if err != nil {
			t.Fatalf("generated Orb name cannot bind default-length API socket (%d bytes): %v", len(path), err)
		}
		conn, err := net.Dial("unix", path)
		listener.Close()
		if err != nil {
			t.Fatal("cannot dial API socket", err)
		}
		conn.Close()
		h.must("delete", "")
	}
}

func TestLegacyOrbNamesKeepStateAndLock(t *testing.T) {
	for _, scenario := range []string{"managed", "missing-record", "missing-all", "new-record"} {
		t.Run(scenario, func(t *testing.T) {
			h := newHarness(t, "")
			h.must("spawn", "initial-token")
			id := "amp-thread/with spaces and 'quotes'"
			// Independent reproduction of the previous naming algorithm.
			legacyName := fmt.Sprintf("orb-%x", sha256.Sum256([]byte(id)))[:60]
			s := h.read()
			oldPath := filepath.Join(h.dir, "state", s.VM.Name+".json")
			legacyPath := filepath.Join(h.dir, "state", legacyName+".json")
			data, err := os.ReadFile(oldPath)
			var r record
			if err != nil || json.Unmarshal(data, &r) != nil {
				t.Fatal("cannot read state", err)
			}
			r.Name, s.VM.Name = legacyName, legacyName
			s.Disk = "legacy workspace"
			h.write(s)
			if err := os.Rename(oldPath+".lock", legacyPath+".lock"); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(oldPath); err != nil {
				t.Fatal(err)
			}
			if scenario != "managed" {
				if scenario == "missing-all" {
					if err := os.Remove(legacyPath + ".lock"); err != nil {
						t.Fatal(err)
					}
				} else if scenario == "new-record" {
					r = record{OrbID: id, Endpoint: "srv", Name: legacyName, Phase: "new"}
					if err := save(legacyPath, r); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := h.run("spawn", "blocked"); err == nil {
					t.Fatal("lost legacy record was recreated")
				}
				if current := h.read(); current.CreateAttempts != s.CreateAttempts || current.VM.ID != s.VM.ID || current.Disk != "legacy workspace" {
					t.Fatal("state loss submitted a create or changed existing compute", current.CreateAttempts)
				}
				return
			}
			if err := save(legacyPath, r); err != nil {
				t.Fatal(err)
			}
			h.must("pause", "")
			h.must("spawn", "resume-token")
			if s := h.read(); s.Creates != 1 || s.VM.Name != legacyName || s.Disk != "legacy workspace" {
				t.Fatal("legacy instance was renamed or recreated")
			}
			h.must("delete", "")
			h.must("spawn", "fresh-token")
			if s := h.read(); s.Creates != 2 || len(s.VM.Name) > 53 {
				t.Fatal("legacy deletion did not produce a shorter successor")
			}
			if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
				t.Fatal("provider root record moved", err)
			}
		})
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
			h.must("spawn", "later-token", "SRV_ORB_HOST="+host)
			if s := h.read(); s.VM.Name == name || s.Creates != 2 || s.Disk != "fresh disk" {
				t.Fatal("spawn after delete did not create a distinct fresh workspace")
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

func TestDispatchResumesSameDeleteIntent(t *testing.T) {
	for _, failure := range []string{"before-submit", "preflight"} {
		t.Run(failure, func(t *testing.T) {
			h := newHarness(t, "")
			h.must("spawn", "token")
			s := h.read()
			name := s.VM.Name
			s.RejectDelete = true
			h.write(s)
			if _, err := h.run("delete", ""); err == nil {
				t.Fatal("expected rejection")
			}
			original := h.read().DeleteRequestID
			if original == "" {
				t.Fatal("no bound request")
			}
			s = h.read()
			s.RejectDelete = false
			// v3 remains exercised by the other lifecycle tests.
			s.ProofVersion = 4
			if failure == "before-submit" {
				s.DeleteRequestID = ""
				s.ProofVersion = 5
			}
			s.LoseDelete = true
			h.write(s)
			h.must("delete", "") // A separate receipt lookup confirms the lost reply.
			s = h.read()
			if s.VM != nil || s.Proof["request_id"] != original {
				t.Fatal("retry lost binding or failed to delete")
			}
			h.must("delete", "")
			data, err := os.ReadFile(filepath.Join(h.dir, "state", name+".json"))
			var r record
			if err != nil || json.Unmarshal(data, &r) != nil || r.Phase != "deleted" || r.RequestID != original {
				t.Fatal("not finalized", string(data), err)
			}
			h.must("spawn", "after-cleanup-token")
			if s := h.read(); s.Creates != 2 || s.VM.Name == name || s.Disk != "fresh disk" {
				t.Fatal("valid v4/v5 receipt did not permit fresh compute")
			}
		})
	}
}

func TestDispatchProviderProxyBypass(t *testing.T) {
	// Unset keeps the payload byte-compatible with older backends and guest
	// images: no proxy_bypass key is sent at all.
	h := newHarness(t, "")
	h.must("spawn", "token")
	if s := h.read(); s.ProxyBypass != nil {
		t.Fatalf("unset SRV_ORB_PROXY_BYPASS was sent: %q", *s.ProxyBypass)
	}
	h.must("spawn", "fresh-token", "SRV_ORB_PROXY_BYPASS=.tail1234.ts.net")
	if s := h.read(); s.ProxyBypass == nil || *s.ProxyBypass != ".tail1234.ts.net" {
		t.Fatalf("provider bypass destination not forwarded: %+v", s.ProxyBypass)
	}
	h.must("spawn", "list-token", "SRV_ORB_PROXY_BYPASS=.corp.example,.tail1234.ts.net")
	if s := h.read(); s.ProxyBypass == nil || *s.ProxyBypass != ".corp.example,.tail1234.ts.net" {
		t.Fatalf("provider bypass list not forwarded verbatim: %+v", s.ProxyBypass)
	}
	h.must("spawn", "fqdn-token", "SRV_ORB_PROXY_BYPASS=.example.com.")
	if s := h.read(); s.ProxyBypass == nil || *s.ProxyBypass != ".example.com." {
		t.Fatalf("FQDN bypass destination not forwarded verbatim: %+v", s.ProxyBypass)
	}
	nine := strings.Join([]string{"e1.example", "e2.example", "e3.example", "e4.example", "e5.example", "e6.example", "e7.example", "e8.example", "e9.example"}, ",")
	oversize := strings.TrimSuffix(strings.Repeat("e"+strings.Repeat("x", 61)+".example,", 8), ",")
	for _, invalid := range []string{"", "a b", "a*b", `a"b`, "a\\b", "a\nb", "a:b", "a/b", "a?b", "a#b", "a@b", ".", "..", "-", "_", "http://internal", strings.Repeat("x", 254),
		",a", "a,", "a,,b", "a, b", ".x.com,.x.com", "localhost", ".ts.net", nine, oversize} {
		// Validation runs in the bootstrap payload builder, so it fails after
		// inventory but before any credential SSH exchange: a bootstrapped orb
		// keeps its generation and never sees the invalid value.
		h := newHarness(t, "")
		h.must("spawn", "first")
		before := h.read()
		out, err := h.run("spawn", "second", "SRV_ORB_PROXY_BYPASS="+invalid)
		if err == nil || !strings.Contains(out, "SRV_ORB_PROXY_BYPASS") {
			t.Fatalf("invalid SRV_ORB_PROXY_BYPASS %q accepted: %v %s", invalid, err, out)
		}
		after := h.read()
		if after.VM == nil || after.VM.Generation != before.VM.Generation || after.ProxyBypass != nil {
			t.Fatalf("invalid SRV_ORB_PROXY_BYPASS %q reached bootstrap", invalid)
		}
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

func TestExternalDeleteCompute(t *testing.T) {
	for _, scenario := range []string{"running", "paused", "lost-reply", "pending-bootstrap", "pending-stop", "pending-delete", "failed-delete"} {
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
			if scenario == "pending-stop" {
				s.LateStop = true
				h.write(s)
				if _, err := h.run("pause", ""); err == nil {
					t.Fatal("expected uncertain stop")
				}
				s = h.read()
			}
			s.Disk = "work deliberately discarded"
			s.OtherDown = host != "" // Reconciliation must use the recorded backend.
			s.LoseDelete = scenario == "lost-reply"
			s.RejectDelete = scenario == "failed-delete"
			h.write(s)
			path := filepath.Join(h.dir, "state", name+".json")
			if scenario == "pending-delete" {
				// Crash after saving provider intent, before submitting it.
				data, err := os.ReadFile(path)
				var r record
				if err != nil || json.Unmarshal(data, &r) != nil {
					t.Fatal("cannot read provider state", err)
				}
				r.Phase, r.RequestID = "deleting", "aaaaaaaa-1234-4234-8234-123456789abc"
				if err := save(path, r); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if out, err := h.ssh("delete", name); (err != nil) != (s.LoseDelete || s.RejectDelete) {
				t.Fatal("unexpected SSH deletion result", err, out)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(before) != string(after) {
				t.Fatal("manual deletion must leave provider state untouched", err)
			}
			if scenario == "failed-delete" {
				// Model a failure after fencing but before receipt publication.
				s = h.read()
				s.VM.State = "deleting"
				h.write(s)
				if _, err := h.run("spawn", "blocked-token"); err == nil || h.read().Creates != 1 {
					t.Fatal("unfinished manual deletion allowed fresh compute")
				}
				s.RejectDelete = false
				h.write(s)
				if out, err := h.ssh("delete", name); err != nil {
					t.Fatal("manual deletion retry failed", err, out)
				}
			}
			s = h.read()
			s.FailGuest, s.LateStop = false, false
			h.write(s)
			if scenario == "paused" {
				h.must("pause", "") // Reconcile without allocating anything.
				h.must("pause", "")
			} else if scenario == "lost-reply" {
				h.must("delete", "")
				h.must("delete", "")
			}
			if h.read().VM != nil {
				t.Fatal("pause/delete recreated manually deleted compute")
			}
			h.must("spawn", "new-token", "SRV_ORB_HOST="+host)
			h.must("spawn", "repeat-token")
			if s := h.read(); s.Creates != 2 || s.VM.ID == id || s.VM.Name == name || s.Disk != "fresh disk" {
				t.Fatal("manual deletion did not yield exactly one fresh successor")
			}
			data, err := os.ReadFile(path + ".cleanup")
			var proof map[string]any
			if err != nil || json.Unmarshal(data, &proof) != nil || proof["instance_id"] != id || proof["request_id"] != s.Proof["request_id"] {
				t.Fatal("external cleanup receipt was not retained", err)
			}
		})
	}
}

func TestExternalDeleteRequiresBoundReceipt(t *testing.T) {
	for _, failure := range []string{"missing", "backend", "uuid", "created-at", "partial", "unreachable", "name-reused", "local-write"} {
		t.Run(failure, func(t *testing.T) {
			h := newHarness(t, "")
			h.must("spawn", "old-token")
			old := *h.read().VM
			path := filepath.Join(h.dir, "state", old.Name+".json")
			if out, err := h.ssh("delete", old.Name); err != nil {
				t.Fatal(err, out)
			}
			original := h.read()
			s := h.read()
			switch failure {
			case "missing":
				s.Proof = nil
			case "backend":
				s.Proof["backend_id"] = "other-backend"
			case "uuid":
				s.Proof["instance_id"] = "22222222-2222-4222-8222-222222222222"
			case "created-at":
				s.Proof["created_at"] = "2025-01-02T03:04:05Z"
			case "partial":
				s.Proof["storage_absent"] = false
			case "unreachable":
				s.Unreachable = true
			case "name-reused":
				s.VM = &old
				s.VM.ID = "22222222-2222-4222-8222-222222222222"
				s.Disk = "another instance must not be touched"
			case "local-write":
				if err := os.Mkdir(path+".cleanup", 0700); err != nil {
					t.Fatal(err)
				}
			}
			h.write(s)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := h.run("spawn", "blocked-token"); err == nil {
				t.Fatal("unproven deletion or lost local receipt accepted")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(before) || h.read().Creates != 1 {
				t.Fatal("failed reconciliation replaced the recorded incarnation", err)
			}
			if failure == "name-reused" && (h.read().VM.ID != s.VM.ID || h.read().Disk != s.Disk) {
				t.Fatal("replacement with reused name was changed")
			}
			if failure == "local-write" {
				if err := os.Remove(path + ".cleanup"); err != nil {
					t.Fatal(err)
				}
			}
			h.write(original)
			h.must("spawn", "recovered-token")
			h.must("spawn", "repeated-token")
			if s := h.read(); s.Creates != 2 || s.VM.Name == old.Name || s.Disk != "fresh disk" {
				t.Fatal("retry did not leave exactly one fresh successor")
			}
		})
	}
}

func TestRetireCommandRemoved(t *testing.T) {
	for _, scenario := range []string{"command", "env-only", "unknown-orb"} {
		t.Run(scenario, func(t *testing.T) {
			h := newHarness(t, "")
			h.must("spawn", "token")
			s := h.read()
			args := []string{"retire", "amp-thread/with spaces and 'quotes'", s.VM.ID}
			operation := "delete"
			switch scenario {
			case "env-only":
				args, operation = nil, "retire"
			case "unknown-orb":
				args[1] = "unknown-orb"
			}
			before, err := os.ReadFile(filepath.Join(h.dir, "server.json"))
			if err != nil {
				t.Fatal(err)
			}
			// A malformed operator invocation must not fall back to an Amp
			// delete job inherited from the environment.
			if out, err := h.runArgs(args, operation, ""); err == nil {
				t.Fatal("removed retirement command succeeded", out)
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

func TestFreshComputeRejectsWaitingProviderJobs(t *testing.T) {
	for _, operation := range []string{"spawn", "pause", "delete"} {
		t.Run(operation, func(t *testing.T) {
			h := newHarness(t, "b")
			h.must("spawn", "first-token", "SRV_ORB_HOST=b")
			old := h.read().VM
			orbID := "amp-thread/with spaces and 'quotes'"
			h.must("delete", "")
			path := filepath.Join(h.dir, "state", old.Name+".json")
			lock, err := os.OpenFile(path+".lock", os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
				t.Fatal(err)
			}
			fd, err := unix.InotifyInit1(unix.IN_CLOEXEC)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(fd)
			if _, err := unix.InotifyAddWatch(fd, path+".lock", unix.IN_OPEN); err != nil {
				t.Fatal(err)
			}
			result := make(chan error, 1)
			go func() {
				out, err := h.run(operation, "stale-token")
				if err == nil || !strings.Contains(out, "refusing a stale local job") {
					result <- fmt.Errorf("expected stale-job rejection, got %v: %s", err, out)
					return
				}
				result <- nil
			}()
			// Opening the lock follows reading the admitted identity. Observe
			// that event while holding the lock, rather than guessing with sleeps.
			poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
			if n, err := unix.Poll(poll, 10000); err != nil || n != 1 {
				t.Fatalf("provider did not reach the held lock: %d, %v", n, err)
			}
			// Simulate a concurrent spawn having published its successor while
			// the old job waits. It must not bootstrap, stop or delete that VM.
			if err := save(path, record{OrbID: orbID, Endpoint: "srv", Name: orbName(orbID, old.ID), Host: "b", Phase: "new", PreviousInstanceID: old.ID}); err != nil {
				t.Fatal(err)
			}
			read := func(path string) string {
				t.Helper()
				b, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				return string(b)
			}
			serverPath := filepath.Join(h.dir, "server.json")
			wantState, wantServer := read(path), read(serverPath)
			if err := unix.Flock(int(lock.Fd()), unix.LOCK_UN); err != nil {
				t.Fatal(err)
			}
			if err := <-result; err != nil {
				t.Fatal(err)
			}
			if read(path) != wantState || read(serverPath) != wantServer {
				t.Fatal("stale provider job changed successor state or backend")
			}
		})
	}
}

func TestFreshComputeLifecycle(t *testing.T) {
	for _, template := range []bool{false, true} {
		t.Run(fmt.Sprint(template), func(t *testing.T) {
			h := newHarness(t, "b")
			h.must("spawn", "first-token", "SRV_ORB_HOST=b")
			statePath := filepath.Join(h.dir, "state", h.read().VM.Name+".json")
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
				if cycle == 0 {
					if out, err := h.ssh("delete", oldName); err != nil {
						t.Fatal(err, out)
					}
				} else {
					h.must("delete", "")
				}
				// Reconcile both entry points without creating compute yet.
				h.must("delete", "")
				h.must("delete", "")
				if h.read().VM != nil {
					t.Fatal("delete retry recreated compute")
				}
				proofPath := filepath.Join(h.dir, "state", oldName+".json.cleanup")
				proof, err := os.ReadFile(proofPath)
				if err != nil {
					t.Fatal(err)
				}
				h.must("spawn", "fresh-token", snapshot)
				s = h.read()
				if s.Creates != cycle+2 || s.VM.ID == oldID || s.VM.Name == oldName || s.VM.Generation != 1 || s.Disk != wantDisk || !strings.Contains(s.Environment, `ORB_TOKEN="fresh-token"`) {
					t.Fatalf("fresh spawn did not create an independent workspace: %+v", s.VM)
				}
				// A damaged old archive must not block a healthy successor. No
				// archive is created by the new lifecycle in the first place.
				archive := statePath + ".retired-" + oldID
				if _, err := os.Stat(archive); !os.IsNotExist(err) {
					t.Fatal("unexpected predecessor archive", err)
				}
				if err := os.WriteFile(archive, []byte("legacy damaged archive"), 0600); err != nil {
					t.Fatal(err)
				}
				newID := s.VM.ID
				s.Disk = "new work survives resume"
				h.write(s)
				h.must("spawn", "repeat-token")
				h.must("pause", "")
				if err := os.Remove(archive); err != nil {
					t.Fatal(err)
				}
				h.must("spawn", "resume-token")
				s = h.read()
				if s.Creates != cycle+2 || s.VM.ID != newID || s.Disk != "new work survives resume" {
					t.Fatal("fresh workspace was reset instead of resumed")
				}
				retained, err := os.ReadFile(proofPath)
				if err != nil || string(retained) != string(proof) {
					t.Fatal("prior cleanup receipt was overwritten", err)
				}
			}
			h.must("delete", "")
		})
	}
}

func TestSpawnResumesDeletion(t *testing.T) {
	for _, phase := range []string{"deleting", "retiring"} {
		t.Run(phase, func(t *testing.T) {
			h := newHarness(t, "")
			h.must("spawn", "token")
			s := h.read()
			old := *s.VM
			s.RejectDelete = true
			h.write(s)
			if _, err := h.run("delete", ""); err == nil {
				t.Fatal("expected cleanup failure")
			}
			request := h.read().DeleteRequestID
			path := filepath.Join(h.dir, "state", old.Name+".json")
			data, err := os.ReadFile(path)
			var r record
			if err != nil || json.Unmarshal(data, &r) != nil {
				t.Fatal("cannot read pending deletion", err)
			}
			r.Phase = phase // Also exercise records left by older dispatchers.
			if err := save(path, r); err != nil {
				t.Fatal(err)
			}
			if _, err := h.run("spawn", "blocked-token"); err == nil {
				t.Fatal("spawn ignored cleanup failure")
			}
			s = h.read()
			if s.Creates != 1 || s.VM.ID != old.ID || s.VM.Generation != old.Generation || s.DeleteRequestID != request {
				t.Fatal("failed cleanup changed compute or its owning request")
			}
			s.RejectDelete, s.LoseDelete = false, true
			h.write(s)
			h.must("spawn", "lost-reply-token")
			if s := h.read(); s.Creates != 2 || s.Proof["request_id"] != request {
				t.Fatal("spawn did not finish the original deletion before replacement")
			}
			h.must("spawn", "repeat-token")
			if s := h.read(); s.Creates != 2 || s.VM.ID == old.ID || s.VM.Name == old.Name || s.Disk != "fresh disk" {
				t.Fatal("cleanup retry did not leave exactly one successor")
			}
		})
	}
}

func TestFreshComputeRequiresCleanupProof(t *testing.T) {
	for _, scenario := range []string{"missing-proof", "partial-proof", "wrong-id", "wrong-request", "missing-binding"} {
		t.Run(scenario, func(t *testing.T) {
			h := newHarness(t, "")
			h.must("spawn", "token")
			s := h.read()
			name := s.VM.Name
			path := filepath.Join(h.dir, "state", name+".json")
			h.must("delete", "")
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			s = h.read()
			switch scenario {
			case "wrong-id":
				s.Proof["instance_id"] = "22222222-2222-4222-8222-222222222222"
			case "wrong-request":
				s.Proof["request_id"] = "22222222-2222-4222-8222-222222222222"
			case "missing-proof":
				if err := os.Remove(path + ".cleanup"); err != nil {
					t.Fatal(err)
				}
			case "partial-proof":
				delete(s.Proof, "gateways_absent")
			case "missing-binding":
				data, err := os.ReadFile(path)
				var r record
				if err != nil || json.Unmarshal(data, &r) != nil {
					t.Fatal("cannot read deletion", err)
				}
				r.InstanceID, r.RequestID = "", ""
				if err := save(path, r); err != nil {
					t.Fatal(err)
				}
			}
			if scenario != "missing-proof" {
				if err := save(path+".cleanup", s.Proof); err != nil {
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
			if out, err := h.run("spawn", "blocked-token"); err == nil {
				t.Fatal("unproven fresh compute accepted", out)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(before) != string(after) {
				t.Fatal("rejection changed current incarnation", err)
			}
			after, err = os.ReadFile(filepath.Join(h.dir, "server.json"))
			if err != nil || string(server) != string(after) {
				t.Fatal("rejection changed backend state", err)
			}
			// Restoring the original evidence permits the same spawn to retry.
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			if err := save(path+".cleanup", h.read().Proof); err != nil {
				t.Fatal(err)
			}
			h.must("spawn", "recovered-token")
			if s := h.read(); s.Creates != 2 || s.Disk != "fresh disk" {
				t.Fatal("restored evidence did not permit fresh compute")
			}
		})
	}
}

func TestFreshComputeRecovery(t *testing.T) {
	for _, scenario := range []string{"legacy-retired", "published", "unreachable", "lost-create", "rejected-create", "lost-bootstrap"} {
		t.Run(scenario, func(t *testing.T) {
			h := newHarness(t, "b")
			h.must("spawn", "first-token", "SRV_ORB_HOST=b")
			old := *h.read().VM
			path := filepath.Join(h.dir, "state", old.Name+".json")
			h.must("delete", "")
			data, err := os.ReadFile(path)
			var r record
			if err != nil || json.Unmarshal(data, &r) != nil {
				t.Fatal("cannot read completed deletion", err)
			}
			if scenario == "legacy-retired" {
				r.Phase = "retired"
			} else if scenario == "published" {
				// Crash after durable successor publication, before create. This
				// is also the state left by the removed fresh-start command.
				r = record{OrbID: r.OrbID, Endpoint: "srv", Name: orbName(r.OrbID, old.ID), Host: "b", Phase: "new", PreviousInstanceID: old.ID}
			}
			if err := save(path, r); err != nil {
				t.Fatal(err)
			}
			if scenario == "legacy-retired" {
				if out, err := h.runArgs([]string{"fresh-start", r.OrbID, old.ID}, "delete", ""); err == nil || !strings.Contains(out, "usage:") {
					t.Fatal("removed command accepted or fell through to inherited job", err, out)
				}
				if _, err := h.run("fresh-start", ""); err == nil {
					t.Fatal("removed operation accepted through environment")
				}
			}
			s := h.read()
			s.Unreachable = scenario == "unreachable"
			s.LoseCreate = scenario == "lost-create"
			s.RejectCreate = scenario == "rejected-create"
			s.LoseBootstrap = scenario == "lost-bootstrap"
			h.write(s)
			out, err := h.run("spawn", "new-token", "SRV_ORB_HOST=b")
			if (err != nil) != (scenario != "legacy-retired" && scenario != "published") {
				t.Fatal("unexpected first spawn result", err, out)
			}
			s = h.read()
			s.Unreachable, s.LoseCreate, s.RejectCreate, s.LoseBootstrap = false, false, false, false
			h.write(s)
			if scenario == "lost-create" || scenario == "rejected-create" {
				if _, err := h.run("spawn", "retry-token"); err == nil || h.read().Creates != s.Creates {
					t.Fatal("uncertain successor creation was adopted or retried")
				}
				if scenario == "rejected-create" {
					if s.VM != nil || s.Creates != 1 {
						t.Fatal("rejected create unexpectedly allocated a successor")
					}
					return // Unknown outcome remains fenced; no safe automatic retry.
				}
				if out, err := h.runArgs([]string{"bind", r.OrbID, s.VM.ID}, "", ""); err != nil {
					t.Fatal("cannot bind reconciled successor", err, out)
				}
			}
			h.must("spawn", "retry-token")
			s = h.read()
			if s.Creates != 2 || s.VM.ID == old.ID || s.VM.Name == old.Name || s.Disk != "fresh disk" || !strings.Contains(s.Environment, `ORB_TOKEN="retry-token"`) {
				t.Fatal("retry did not leave one independently bound successor")
			}
		})
	}
}

func TestDeleteBeforeFirstSpawn(t *testing.T) {
	for _, phase := range []string{"current", "deleted", "new"} {
		t.Run(phase, func(t *testing.T) {
			h := newHarness(t, "")
			h.must("delete", "")
			h.must("delete", "")
			if phase != "current" {
				id := "amp-thread/with spaces and 'quotes'"
				name := fmt.Sprintf("orb-%x", sha256.Sum256([]byte(id)))[:60]
				r := record{OrbID: id, Endpoint: "srv", Name: name, Phase: phase}
				path := filepath.Join(h.dir, "state", orbName(id, "")+".json")
				legacyPath := filepath.Join(h.dir, "state", name+".json")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(path+".lock", legacyPath+".lock"); err != nil {
					t.Fatal(err)
				}
				if err := save(legacyPath, r); err != nil {
					t.Fatal(err)
				}
			}
			h.must("spawn", "first-token")
			h.must("spawn", "repeat-token")
			if s := h.read(); s.Creates != 1 || s.Disk != "fresh disk" || len(s.VM.Name) > 53 {
				t.Fatal("delete of unallocated orb prevented or duplicated creation")
			}
		})
	}
}

func TestFreshComputeDoesNotClaimOldSnapshots(t *testing.T) {
	for _, phase := range []string{"ready", "creating", "legacy-creating"} {
		t.Run(phase, func(t *testing.T) {
			h := newHarness(t, "")
			h.must("spawn", "token")
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
			if out, err := h.ssh("delete", h.read().VM.Name); err != nil {
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
