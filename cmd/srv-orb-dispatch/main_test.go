package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Image     string `json:"image"`
	State     string `json:"state"`
	Proxy     string `json:"amp_connect_gateway"`
	Migration string `json:"migration_phase"`
}

type testServer struct {
	VM           *testVM
	Disk         string
	Templates    map[string]string
	LoseTemplate bool
	Environment  string
	Host         string
	Unreachable  bool
	OtherDown    bool
	LoseCreate   bool
	RejectCreate bool
	LoseStop     bool
	LoseDelete   bool
	BadInventory bool
	FailGuest    bool
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
	switch args[1] {
	case "bootstrap":
		var req map[string]string
		payload, err := io.ReadAll(os.Stdin)
		if err != nil || json.Unmarshal(payload, &req) != nil || req["token"] == "" || s.VM == nil || args[2] != s.VM.Name {
			return fmt.Errorf("invalid bootstrap")
		}
		if strings.Contains(command, req["token"]) {
			return fmt.Errorf("token leaked in argv")
		}
		s.Environment = fmt.Sprintf("ORB_TOKEN=%q\nAMP_URL=%q\nHTTPS_PROXY=%q", req["token"], req["amp_url"], req["proxy"])
		if s.FailGuest {
			return fmt.Errorf("guest accidentally printed %s", s.Environment)
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
		s.VM = &testVM{Name: args[2], Kind: "orb", Image: "amp-orb", State: "ready", Proxy: "http://10.0.0.1:8080"}
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
		if s.VM.State == "stopped" {
			return fmt.Errorf("already stopped")
		}
		s.VM.State = "stopped"
		if err := write(); err != nil {
			return err
		}
		if s.LoseStop {
			return fmt.Errorf("connection lost after stop")
		}
	case "start":
		if s.VM.State != "stopped" {
			return fmt.Errorf("already running")
		}
		s.VM.State = "ready"
	case "delete":
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
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.program)
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
			if _, err := h.run("spawn", "token"); err == nil {
				t.Fatal("create failure not reported")
			}
			s = h.read()
			s.RejectCreate, s.LoseCreate = false, false
			h.write(s)
			if accepted {
				h.must("spawn", "fresh-token")
				s = h.read()
				s.Disk = "work saved before pause"
				s.LoseStop = true
				h.write(s)
				if _, err := h.run("pause", ""); err == nil {
					t.Fatal("lost stop response not reported")
				}
				h.must("pause", "") // Confirm stopped state after lost response.
				s = h.read()
				if s.VM.State != "stopped" || s.Disk != "work saved before pause" {
					t.Fatal("pause retry did not preserve the stopped VM and its disk")
				}
				s.LoseDelete = true
				h.write(s)
				if _, err := h.run("delete", ""); err == nil {
					t.Fatal("lost delete response not reported")
				}
				h.must("delete", "") // Confirm absence after lost response.
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
