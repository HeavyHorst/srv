// srv-orb-dispatch adapts Amp's self-hosted orb jobs to srv's SSH commands.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
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

	"golang.org/x/sys/unix"
)

type config struct {
	endpoint, directory, image, host, cpus, ram, disk, ssh string
}

type job struct {
	operation, id, image, token, ampURL, snapshot string
}

// A creating record is written BEFORE sending new. An uncertain create is never
// retried on another host, even if the VM is not yet visible in list.
type record struct {
	OrbID    string `json:"orb_id"`
	Endpoint string `json:"endpoint"`
	Name     string `json:"name"`
	Host     string `json:"host,omitempty"`
	Phase    string `json:"phase"`
}

type instance struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Image     string `json:"image"`
	State     string `json:"state"`
	Migration string `json:"migration_phase"`
	Proxy     string `json:"amp_connect_gateway"`
}

func main() {
	c := config{
		endpoint: envDefault("SRV_ORB_ENDPOINT", "srv"), directory: os.Getenv("SRV_ORB_STATE_DIR"),
		image: envDefault("SRV_ORB_IMAGE", "amp-orb"), host: os.Getenv("SRV_ORB_HOST"),
		cpus: envDefault("SRV_ORB_CPUS", "2"), ram: envDefault("SRV_ORB_RAM", "4G"),
		disk: envDefault("SRV_ORB_DISK", "20G"), ssh: "ssh",
	}
	j := job{os.Getenv("ORB_OPERATION"), os.Getenv("ORB_ID"), os.Getenv("ORB_IMAGE"), os.Getenv("ORB_TOKEN"), os.Getenv("AMP_URL"), os.Getenv("ORB_SNAPSHOT_ID")}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 270*time.Second)
	defer cancel()
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

func dispatch(ctx context.Context, c config, j job) error {
	if j.id == "" || (j.operation != "spawn" && j.operation != "pause" && j.operation != "delete") {
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
	if err := os.MkdirAll(c.directory, 0700); err != nil {
		return err
	}
	// Hashing keeps arbitrary upstream IDs out of paths and within srv's name limit.
	name := fmt.Sprintf("orb-%x", sha256.Sum256([]byte(j.id)))[:60]
	path := filepath.Join(c.directory, name+".json")
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
		if r.OrbID != j.id || r.Endpoint != c.endpoint || r.Name != name {
			return errors.New("state identity/endpoint mismatch; restore the original provider configuration")
		}
		switch r.Phase {
		case "creating", "managed", "deleting", "deleted":
		default:
			return errors.New("invalid dispatch record phase")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
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
	if r.Phase == "deleted" {
		if j.operation == "spawn" {
			return errors.New("orb was deleted; refusing to recreate its lost disk")
		}
		return nil
	}
	if r.Phase == "deleting" && j.operation != "delete" {
		return errors.New("deletion is pending; repeat delete before any other operation")
	}
	if r.Phase == "" && j.operation == "spawn" && snapshot != nil {
		if snapshot.Phase != "ready" || snapshot.Image != c.image {
			return errors.New("snapshot is incomplete or belongs to a different image; repair or retire its mapping")
		}
		r.Host = snapshot.Host
	}
	v, host, err := locate(ctx, c, r)
	if err != nil {
		return err // An unreachable host is never evidence of absence.
	}
	if v != nil && r.Phase == "" {
		return errors.New("VM already exists without a dispatch record; refusing to adopt it")
	}
	if v == nil {
		switch r.Phase {
		case "creating":
			return errors.New("create outcome is unknown; inspect all backends before repairing the dispatch record; new was not retried")
		case "managed", "deleting":
			if j.operation != "delete" {
				return errors.New("recorded VM is missing; refusing to replace its disk")
			}
		}
		if j.operation == "delete" {
			r.Phase = "deleted"
			return save(path, r)
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
	}
	if v.Kind != "orb" {
		return errors.New("recorded instance is not an orb; legacy Tailscale VMs require explicit operator migration")
	}
	if v.Migration != "" && v.Migration != "aborted" {
		return errors.New("orb has migration state; resolve migration and host mapping manually")
	}
	r.Host = host
	if r.Phase != "deleting" {
		r.Phase = "managed"
	}
	if err := save(path, r); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "srv-orb: %s %s (host %q)\n", j.operation, r.Name, r.Host)
	switch j.operation {
	case "pause":
		if v.State != "stopped" {
			if _, err = control(ctx, c, r, "stop", r.Name); err != nil {
				return err
			}
		}
		if j.snapshot != "" {
			return captureSnapshot(ctx, c, j, r, v.Image, snapshot)
		}
		return nil
	case "delete":
		r.Phase = "deleting"
		if err := save(path, r); err != nil {
			return err
		}
		if _, err := control(ctx, c, r, "delete", r.Name); err != nil {
			return err
		}
		r.Phase = "deleted"
		return save(path, r)
	}
	if v.State == "stopped" {
		if _, err := control(ctx, c, r, "start", r.Name); err != nil {
			return err
		}
	}
	for {
		data, err := control(ctx, c, r, "inspect", r.Name)
		if err != nil {
			return err
		}
		var response struct{ Instance instance }
		if err := json.Unmarshal(data, &response); err != nil {
			return err
		}
		v = &response.Instance
		if v.Name != r.Name {
			return errors.New("inspect returned a different instance")
		}
		switch v.State {
		case "ready":
			payload, err := json.Marshal(map[string]string{"token": j.token, "amp_url": j.ampURL, "proxy": v.Proxy})
			if err != nil {
				return err
			}
			args := []string{"orb", "bootstrap", r.Name}
			if r.Host != "" {
				args = append(args, "--host", r.Host)
			}
			for i := range args {
				args[i] = quote(args[i])
			}
			if _, err := ssh(ctx, c, c.endpoint, strings.Join(args, " "), strings.NewReader(string(payload))); err != nil {
				return errors.New("orb bootstrap failed; inspect the backend and guest bootstrap service")
			}
			return nil
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
		if key != "AMP_ORB_PROVIDER_TOKEN" && key != "ORB_TOKEN" {
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
			if v.Name == r.Name {
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
