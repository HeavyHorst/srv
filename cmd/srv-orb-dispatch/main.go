// srv-orb-dispatch adapts Amp's self-hosted orb jobs to srv's SSH commands.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
	"srv/internal/model"
)

type config struct {
	endpoint, directory, image, host, cpus, ram, disk, ssh string
}

type job struct {
	operation, id, image, token, ampURL, snapshot string
	expected                                      string
}

// A creating record is written BEFORE sending new. An uncertain create is never
// retried on another host, even if the VM is not yet visible in list.
type record struct {
	OrbID               string `json:"orb_id"`
	Endpoint            string `json:"endpoint"`
	Name                string `json:"name"`
	Host                string `json:"host,omitempty"`
	Phase               string `json:"phase"`
	PreviousInstanceID  string `json:"previous_instance_id,omitempty"`
	InstanceID          string `json:"instance_id,omitempty"`
	CreatedAt           string `json:"created_at,omitempty"`
	BackendID           string `json:"backend_id,omitempty"`
	Image               string `json:"image,omitempty"`
	Pending             string `json:"pending,omitempty"`
	RequestID           string `json:"request_id,omitempty"`
	BootstrapGeneration *int64 `json:"bootstrap_generation"`
	BootstrapFloor      int64  `json:"bootstrap_floor"`
}

type instance struct {
	ID                  string `json:"id"`
	CreatedAt           string `json:"created_at"`
	BackendID           string `json:"backend_id"`
	BootstrapGeneration *int64 `json:"bootstrap_generation"`
	Imported            string `json:"import_phase"`
	Name                string `json:"name"`
	Kind                string `json:"kind"`
	Image               string `json:"image"`
	State               string `json:"state"`
	Migration           string `json:"migration_phase"`
	Proxy               string `json:"amp_connect_gateway"`
}

func main() {
	c := config{
		endpoint: envDefault("SRV_ORB_ENDPOINT", "srv"), directory: os.Getenv("SRV_ORB_STATE_DIR"),
		image: envDefault("SRV_ORB_IMAGE", "amp-orb"), host: os.Getenv("SRV_ORB_HOST"),
		cpus: envDefault("SRV_ORB_CPUS", "2"), ram: envDefault("SRV_ORB_RAM", "4G"),
		disk: envDefault("SRV_ORB_DISK", "20G"), ssh: "ssh",
	}
	j := job{operation: os.Getenv("ORB_OPERATION"), id: os.Getenv("ORB_ID"), image: os.Getenv("ORB_IMAGE"), token: os.Getenv("ORB_TOKEN"), ampURL: os.Getenv("AMP_URL"), snapshot: os.Getenv("ORB_SNAPSHOT_ID")}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 270*time.Second)
	defer cancel()
	if len(os.Args) == 4 && (os.Args[1] == "bind" || os.Args[1] == "cancel") {
		j = job{operation: os.Args[1], id: os.Args[2], expected: os.Args[3]}
	} else if len(os.Args) > 1 && !(len(os.Args) == 2 && os.Args[1] == "init") {
		fmt.Fprintln(os.Stderr, "usage: srv-orb-dispatch [init | bind ORB_ID UUID | cancel ORB_ID REQUEST]")
		os.Exit(2)
	}
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		fmt.Fprintln(os.Stderr, "srv-orb: disable core dumps:", err)
		os.Exit(1)
	}
	if len(os.Args) == 2 && os.Args[1] == "init" {
		if err := initialize(c); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if err := dispatch(ctx, c, j); err != nil {
		fmt.Fprintln(os.Stderr, "srv-orb:", err)
		os.Exit(1)
	}
}

func envDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// SRV_ORB_PROXY_BYPASS names the provider-owned destinations appended to the
// guest's built-in NO_PROXY list. It is read and validated only here, inside
// the bootstrap payload builder, and travels only in that payload: never in
// SSH argv, state records, logs, or the SSH child environment. Unset omits
// the field entirely, keeping the payload compatible with older srv backends
// and guest images; an empty value is a configuration error, not "unset".
// The value is a bounded list: one to eight comma-separated entries, at most
// 512 characters joined, each a hostname, an IPv4 literal, or a leading-dot
// DNS suffix with an optional trailing dot (FQDN form) kept verbatim. Empty
// entries, duplicates, built-in repeats, whitespace, CIDR and IPv6 forms are
// rejected: NO_PROXY handling differs between HTTP clients, so this is a
// matching contract, not a DNS-name linter or a policy framework.
func proxyBypass() (string, error) {
	value, configured := os.LookupEnv("SRV_ORB_PROXY_BYPASS")
	if !configured {
		return "", nil
	}
	if value == "" {
		return "", errors.New("SRV_ORB_PROXY_BYPASS must not be empty; unset it to keep the built-in bypass list")
	}
	if err := validateProxyBypassList(value); err != nil {
		return "", err
	}
	return value, nil
}

// The guest's fixed bypass entries. Repeating one in the provider list cannot
// disable it — appending only adds — but it is noise implying a second policy.
var builtinNoProxyTokens = map[string]bool{
	"localhost": true, "127.0.0.1": true, "::1": true,
	"169.254.169.254": true, "100.64.0.0/10": true, ".ts.net": true,
}

func validateProxyBypassList(value string) error {
	if len(value) > 512 {
		return errors.New("SRV_ORB_PROXY_BYPASS must stay within 512 characters")
	}
	if strings.ContainsAny(value, " \t\r\n\x00'\"\\") {
		return errors.New("SRV_ORB_PROXY_BYPASS entries must not contain whitespace, quotes, or backslashes")
	}
	entries := strings.Split(value, ",")
	if len(entries) > 8 {
		return errors.New("SRV_ORB_PROXY_BYPASS accepts at most 8 entries")
	}
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		switch {
		case entry == "":
			return errors.New("SRV_ORB_PROXY_BYPASS must not contain empty entries (leading, trailing, or doubled commas)")
		case !validProxyBypass(entry):
			return errors.New("SRV_ORB_PROXY_BYPASS entries must each be a hostname, IPv4 literal, or leading-dot DNS suffix (no CIDR, IPv6, wildcard, scheme, or port)")
		case builtinNoProxyTokens[entry]:
			return errors.New("SRV_ORB_PROXY_BYPASS must not repeat a built-in bypass entry")
		case seen[entry]:
			return errors.New("SRV_ORB_PROXY_BYPASS must not contain duplicate entries")
		}
		seen[entry] = true
	}
	return nil
}

func validProxyBypass(value string) bool {
	if len(value) > 253 {
		return false
	}
	name := strings.TrimPrefix(value, ".")
	if name == "" {
		return false
	}
	labels := strings.Split(name, ".")
	if labels[len(labels)-1] == "" {
		// A single trailing dot is the valid FQDN form; it is applied
		// verbatim and only matches clients that compare the same form.
		labels = labels[:len(labels)-1]
	}
	if len(labels) == 0 {
		return false
	}
	alphanumeric := false
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 {
			return false
		}
		for _, c := range label {
			switch {
			case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
				alphanumeric = true
			case c == '-', c == '_':
			default:
				return false
			}
		}
	}
	return alphanumeric
}

func dispatch(ctx context.Context, c config, j job) error {
	if j.id == "" || (j.operation != "spawn" && j.operation != "pause" && j.operation != "delete" && j.operation != "bind" && j.operation != "cancel") {
		return errors.New("ORB_ID and ORB_OPERATION=spawn|pause|delete are required")
	}
	if !filepath.IsAbs(c.directory) || c.endpoint == "" || strings.HasPrefix(c.endpoint, "-") {
		return errors.New("set an absolute SRV_ORB_STATE_DIR and a valid SRV_ORB_ENDPOINT")
	}
	if j.operation == "spawn" && (j.token == "" || j.image != c.image) {
		return errors.New("spawn requires ORB_TOKEN and ORB_IMAGE matching SRV_ORB_IMAGE (a prepared srv image name)")
	}
	if strings.ContainsAny(j.token+j.ampURL, "\x00\r\n") {
		return errors.New("invalid bootstrap environment")
	}
	marker, err := os.ReadFile(filepath.Join(c.directory, "provider.json"))
	if err != nil || string(marker) != c.endpoint {
		return errors.New("provider state missing or endpoint changed; restore state, or explicitly init a new provider")
	}
	// Hashing keeps arbitrary upstream IDs out of paths and within srv's name limit.
	name := orbName(j.id, "")
	path := filepath.Join(c.directory, name+".json")
	// Existing providers retain their original root record and lock, including
	// a lock whose record was lost. Never turn missing legacy state into create.
	legacyPath := filepath.Join(c.directory, legacyOrbName(j.id, "")+".json")
	for _, candidate := range []string{legacyPath, legacyPath + ".lock"} {
		if _, err := os.Stat(candidate); err == nil {
			path = legacyPath
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	providerJob := j.operation == "spawn" || j.operation == "pause" || j.operation == "delete"
	admittedName := name
	if providerJob {
		// A local invocation already waiting at the old incarnation must not
		// acquire authority over a successor after it obtains the stable lock.
		if data, err := os.ReadFile(path); err == nil {
			var admitted record
			if err := json.Unmarshal(data, &admitted); err != nil {
				return err
			}
			admittedName = admitted.Name
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	_, lockErr := os.Stat(path + ".lock")
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close() // Do not unlink locks: waiters must share the same inode.
	for {
		err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			return err
		}
		if err := wait(ctx); err != nil {
			return err
		}
	}
	r := record{OrbID: j.id, Endpoint: c.endpoint, Name: name, Host: c.host}
	data, err := os.ReadFile(path)
	if err == nil {
		r = record{} // Missing optional fields must not inherit changed configuration.
		if err := json.Unmarshal(data, &r); err != nil {
			return err
		}
		if r.OrbID != j.id || r.Endpoint != c.endpoint || (r.Name != orbName(j.id, r.PreviousInstanceID) && r.Name != legacyOrbName(j.id, r.PreviousInstanceID)) {
			return errors.New("state identity/endpoint mismatch; restore the original provider configuration")
		}
		switch r.Phase {
		case "new", "creating", "managed", "deleting", "deleted", "retiring", "retired":
		default:
			return errors.New("invalid dispatch record phase")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	} else {
		if lockErr == nil || !errors.Is(lockErr, os.ErrNotExist) {
			return errors.New("dispatch record lost; restore it rather than recreating the orb")
		}
		r.Phase = "new"
		if err := save(path, r); err != nil {
			return err
		}
	}
	if providerJob && r.Name != admittedName {
		return errors.New("orb incarnation changed while this job waited; refusing a stale local job")
	}
	// Read legacy retirement records, but publish only the shared delete states.
	if r.Phase == "retiring" {
		r.Phase = "deleting"
	} else if r.Phase == "retired" {
		r.Phase = "deleted"
	}
	if r.Phase == "deleting" {
		if j.operation != "delete" && j.operation != "spawn" {
			return errors.New("deletion pending; repeat delete or spawn to finish cleanup")
		}
		if err := finishDelete(ctx, c, path, &r); err != nil {
			return err
		}
		r.Phase = "deleted"
	}
	if r.Phase == "deleted" && j.operation != "spawn" {
		return nil
	}
	// Lock ordering is always orb, then snapshot. Different orbs may refer to
	// one template; publication and selection must not race across hosts.
	var snapshot *snapshotRecord
	if j.snapshot != "" {
		unlock, err := lockSnapshot(ctx, c, j.snapshot)
		if err != nil {
			return err
		}
		defer unlock()
		snapshot, err = loadSnapshot(c, j.snapshot)
		if err != nil {
			return err
		}
	}
	var v *instance
	var host string
	for {
		if r.Phase == "deleted" {
			if j.operation != "spawn" {
				return nil
			}
			r, err = freshCompute(c, path, r)
			if err != nil {
				return err
			}
		}
		if r.Phase == "new" && j.operation == "spawn" {
			// No create was submitted yet, including legacy fresh-start records.
			r.Name = orbName(j.id, r.PreviousInstanceID)
			if snapshot != nil {
				if snapshot.Phase != "ready" || snapshot.Image != c.image {
					return errors.New("snapshot is incomplete or belongs to a different image; repair or retire its mapping")
				}
				r.Host = snapshot.Host
			}
		}
		v, host, err = locate(ctx, c, r)
		if err != nil {
			return err // An unreachable host is never evidence of absence.
		}
		if v != nil || r.Phase != "managed" || !providerJob {
			break
		}
		// A manual SSH delete has its own request ID. Only a committed receipt
		// for our exact incarnation can release it, including pending work.
		data, err := control(ctx, c, r, "cleanup-receipt", "--instance", r.InstanceID)
		if err != nil {
			return fmt.Errorf("recorded VM is missing; cleanup receipt unavailable: %w", err)
		}
		var proof model.CleanupReceipt
		if err := json.Unmarshal(data, &proof); err != nil {
			return err
		}
		r.RequestID = proof.RequestID
		if err := saveCleanup(path, r, proof); err != nil {
			return err
		}
		r.Phase = "deleted"
	}
	if r.Pending != "" {
		if j.operation == "cancel" {
			if j.expected != r.RequestID {
				return errors.New("pending request ID mismatch")
			}
			args := append([]string{"operation-cancel", r.RequestID, r.Name, r.Pending}, identity(r)...)
			if _, err := control(ctx, c, r, args...); err != nil {
				return err
			}
		}
		if err := resolveOperation(ctx, c, path, &r); err != nil {
			// Only bootstrap has a downstream generation fence. A fresh spawn
			// may supersede it; start/stop never infer completion from state.
			if r.Pending != "bootstrap" || (j.operation != "spawn" && j.operation != "delete") {
				return err
			}
		}
		if j.operation == "cancel" {
			return nil
		}
	} else if j.operation == "cancel" {
		return errors.New("no pending operation")
	}
	if v != nil && r.Phase == "new" {
		return errors.New("VM already exists without a dispatch record; refusing to adopt it")
	}
	if v == nil {
		switch r.Phase {
		case "creating":
			return errors.New("create outcome is unknown; inspect all backends before repairing the dispatch record; new was not retried")
		case "managed", "deleting":
			return errors.New("recorded VM is missing; absence is not a cleanup receipt")
		}
		if j.operation == "bind" {
			return errors.New("cannot bind an absent orb")
		}
		if j.operation == "delete" {
			return nil // No allocation was attempted; a later spawn may create it.
		}
		if j.operation == "pause" {
			if j.snapshot != "" {
				return errors.New("cannot snapshot an absent orb")
			}
			return nil
		}
		args := []string{"orb", "new", r.Name, "--image", c.image, "--cpus", c.cpus, "--ram", c.ram, "--rootfs-size", c.disk}
		if snapshot != nil {
			args = append(args, "--template", snapshot.Name)
		}
		r.Phase = "creating"
		if err := save(path, r); err != nil {
			return err
		}
		_, err := control(ctx, c, r, args...)
		if err != nil {
			return fmt.Errorf("create may have succeeded; repeat the job to reconcile, not recreate: %w", err)
		}
		v, host, err = locate(ctx, c, r)
		if err != nil {
			return err
		}
		if v == nil {
			return errors.New("created VM not visible yet; repeat the job to reconcile")
		}
		// Only a successful new reply may establish a binding automatically.
		j.expected = "created"
	}
	if j.operation == "bind" && host != r.Host {
		return errors.New("binding requires the original recorded host; ambiguous front allocation cannot be adopted")
	}
	r.Host = host
	v, err = inspect(ctx, c, r)
	if err != nil {
		return err
	}
	if v.Kind != "orb" {
		return errors.New("recorded instance is not an orb; legacy Tailscale VMs require explicit operator migration")
	}
	if v.Imported != "" || v.Migration != "" && v.Migration != "aborted" {
		return errors.New("orb has migration state; resolve migration and host mapping manually")
	}
	if r.InstanceID == "" {
		if j.expected != "created" && (j.operation != "bind" || j.expected != v.ID) {
			return errors.New("orb has no incarnation binding; use explicit bind after reconciling the original create")
		}
		if _, err := uuid.Parse(v.ID); err != nil || v.CreatedAt == "" || v.BackendID == "" || v.Image != c.image {
			return errors.New("incomplete orb identity")
		}
		r.InstanceID, r.CreatedAt, r.BackendID, r.Image = v.ID, v.CreatedAt, v.BackendID, v.Image
		if v.BootstrapGeneration == nil || *v.BootstrapGeneration != 0 {
			return errors.New("cannot initialize a missing bootstrap counter from a used orb")
		}
		r.BootstrapGeneration = new(int64)
	}
	if err := matches(r, v); err != nil {
		return err
	}
	if r.Pending == "bootstrap" && j.operation == "spawn" && v.State != "ready" {
		return errors.New("uncertain bootstrap requires the same running orb; stop/start remains fenced")
	}
	if r.Phase != "deleting" {
		r.Phase = "managed"
	}
	if err := save(path, r); err != nil {
		return err
	}
	if j.operation == "bind" {
		return nil
	}
	fmt.Fprintf(os.Stderr, "srv-orb: %s %s (host %q)\n", j.operation, r.Name, r.Host)
	switch j.operation {
	case "pause":
		if v.State != "stopped" {
			if err = operate(ctx, c, path, &r, "stop"); err != nil {
				return err
			}
		}
		if j.snapshot != "" {
			return captureSnapshot(ctx, c, j, r, v.Image, snapshot)
		}
		return nil
	case "delete":
		r.Phase = "deleting"
		r.RequestID = uuid.NewString()
		if err := save(path, r); err != nil {
			return err
		}
		args := append([]string{"delete", r.Name, "--expect", r.RequestID}, identity(r)...)
		if _, err := control(ctx, c, r, args...); err != nil {
			return err
		}
		return finishDelete(ctx, c, path, &r)
	}
	if v.State == "stopped" {
		if err := operate(ctx, c, path, &r, "start"); err != nil {
			return err
		}
	}
	for {
		v, err = inspect(ctx, c, r)
		if err != nil {
			return err
		}
		if err := matches(r, v); err != nil {
			return err
		}
		switch v.State {
		case "ready":
			if *r.BootstrapGeneration == int64(^uint64(0)>>1) {
				return errors.New("bootstrap generation exhausted")
			}
			bypass, err := proxyBypass()
			if err != nil {
				return err
			}
			r.BootstrapFloor = *v.BootstrapGeneration
			*r.BootstrapGeneration++
			payload := map[string]any{"token": j.token, "amp_url": j.ampURL, "proxy": v.Proxy, "generation": *r.BootstrapGeneration}
			if bypass != "" {
				payload["proxy_bypass"] = bypass
			}
			data, err := json.Marshal(payload)
			if err != nil {
				return err
			}
			if len(data)+1 > 16384 {
				return errors.New("orb bootstrap payload exceeds the backend request limit")
			}
			r.Pending, r.RequestID = "bootstrap", uuid.NewString()
			if err := save(path, r); err != nil {
				return err
			}
			args := append([]string{"orb", "bootstrap", r.Name, "--expect", r.RequestID}, identity(r)...)
			if r.Host != "" {
				args = append(args, "--host", r.Host)
			}
			for i := range args {
				args[i] = quote(args[i])
			}
			if _, err := ssh(ctx, c, c.endpoint, strings.Join(args, " "), strings.NewReader(string(data))); err != nil {
				return errors.New("orb bootstrap failed; inspect the backend and guest bootstrap service")
			}
			return resolveOperation(ctx, c, path, &r)
		case "provisioning", "awaiting-guest":
		default:
			return fmt.Errorf("VM is %q; inspect and repair it before retrying spawn", v.State)
		}
		if err := wait(ctx); err != nil {
			return err
		}
	}
}

func wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Second):
		return nil
	}
}

func save(path string, r any) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".orb-state-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

func ssh(ctx context.Context, c config, destination, command string, input *strings.Reader) ([]byte, error) {
	options := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "-o", "ServerAliveInterval=10", "-o", "ServerAliveCountMax=2"}
	options = append(options, "-o", "StrictHostKeyChecking=yes")
	cmd := exec.CommandContext(ctx, c.ssh, append(options, "--", destination, command)...)
	cmd.WaitDelay = time.Second
	// Do not expose provider credentials to ssh (including SendEnv configurations).
	for _, value := range os.Environ() {
		key, _, _ := strings.Cut(value, "=")
		if key != "AMP_ORB_PROVIDER_TOKEN" && key != "ORB_TOKEN" && key != "SRV_ORB_PROXY_BYPASS" {
			cmd.Env = append(cmd.Env, value)
		}
	}
	if input != nil {
		cmd.Stdin = input
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("SSH failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return data, nil
}

func control(ctx context.Context, c config, r record, args ...string) ([]byte, error) {
	args = append([]string{"--json"}, args...)
	if r.Host != "" {
		args = append(args, "--host", r.Host)
	}
	for i := range args {
		args[i] = quote(args[i])
	}
	return ssh(ctx, c, c.endpoint, strings.Join(args, " "), nil)
}

func locate(ctx context.Context, c config, r record) (*instance, string, error) {
	data, err := control(ctx, c, r, "orb", "list")
	if err != nil {
		return nil, "", err
	}
	type inventory struct {
		Instances *[]instance `json:"instances"`
	}
	var response struct {
		Instances json.RawMessage `json:"instances"`
		Hosts     []struct {
			Host  string    `json:"host"`
			Data  inventory `json:"data"`
			Error string    `json:"error"`
		} `json:"hosts"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, "", err
	}
	var found *instance
	host := r.Host
	check := func(vs []instance, h string) error {
		for _, v := range vs {
			// Before allocation, an existing legacy name is also a conflict.
			// Losing its record and lock must not silently replace its compute.
			if v.Name == r.Name || r.Phase == "new" && v.Name == legacyOrbName(r.OrbID, r.PreviousInstanceID) {
				if found != nil {
					return errors.New("duplicate orb VM names; resolve manually")
				}
				copy := v
				found, host = &copy, h
			}
		}
		return nil
	}
	if response.Instances != nil {
		var vs []instance
		if err := json.Unmarshal(response.Instances, &vs); err != nil {
			return nil, "", err
		}
		if vs == nil {
			return nil, "", errors.New("invalid srv inventory response")
		}
		err = check(vs, r.Host)
	} else if len(response.Hosts) > 0 {
		for _, h := range response.Hosts {
			if h.Error != "" || h.Host == "" || h.Data.Instances == nil {
				return nil, "", errors.New("incomplete backend inventory")
			}
			if err = check(*h.Data.Instances, h.Host); err != nil {
				break
			}
		}
	} else {
		return nil, "", errors.New("invalid srv inventory response")
	}
	return found, host, err
}
