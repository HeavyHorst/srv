// Package front routes authenticated commands without maintaining a VM registry.
package front

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"

	"srv/internal/format"
	"srv/internal/host"
	"srv/internal/identity"
	"srv/internal/model"
)

type Host struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	HostKey string `json:"host_key"`
}
type Config struct {
	Hostname      string             `json:"hostname"`
	DataDir       string             `json:"data_dir"`
	ListenAddr    string             `json:"listen_addr"`
	AllowedUsers  []string           `json:"allowed_users"`
	Hosts         []Host             `json:"hosts"`
	ProviderNodes identity.Providers `json:"provider_nodes,omitempty"`
}

// Transport must terminate streams when ctx is cancelled and report remote exit failures.
type Transport interface {
	Run(context.Context, Host, model.Actor, []string, io.Reader, io.Writer, io.Writer) error
}
type Router struct {
	Hosts     []Host
	Transport Transport
	Timeout   time.Duration
}
type Result struct {
	Host  string          `json:"host"`
	Data  json.RawMessage `json:"data,omitempty"`
	Error string          `json:"error,omitempty"`
}

func (r *Router) query(ctx context.Context, h Host, a model.Actor, args []string) (json.RawMessage, error) {
	d := r.Timeout
	if d == 0 {
		d = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	return r.commandJSON(ctx, h, a, args)
}

func (r *Router) commandJSON(ctx context.Context, h Host, a model.Actor, args []string) (json.RawMessage, error) {
	var b bytes.Buffer
	var e bytes.Buffer
	err := r.Transport.Run(ctx, h, a, args, nil, &b, &e)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, e.String())
	}
	if !json.Valid(b.Bytes()) {
		return nil, fmt.Errorf("invalid backend JSON")
	}
	return b.Bytes(), nil
}
func (r *Router) all(ctx context.Context, a model.Actor, args []string) []Result {
	out := make([]Result, len(r.Hosts))
	var wg sync.WaitGroup
	for i, h := range r.Hosts {
		wg.Add(1)
		go func(i int, h Host) {
			defer wg.Done()
			data, err := r.query(ctx, h, a, args)
			out[i] = Result{Host: h.Name, Data: data}
			if err != nil {
				out[i].Error = err.Error()
			}
		}(i, h)
	}
	wg.Wait()
	return out
}
func (r *Router) host(name string) (Host, error) {
	for _, h := range r.Hosts {
		if h.Name == name {
			return h, nil
		}
	}
	return Host{}, fmt.Errorf("unknown host %q", name)
}
func (r *Router) resolve(ctx context.Context, a model.Actor, name string) (Host, string, error) {
	if h, n, ok := strings.Cut(name, "/"); ok {
		host, err := r.host(h)
		if n == "" || strings.Contains(n, "/") {
			err = fmt.Errorf("invalid canonical name")
		}
		return host, n, err
	}
	var matches []Host
	for _, res := range r.all(ctx, a, []string{"--json", "list", "--all"}) {
		if res.Error != "" {
			return Host{}, "", fmt.Errorf("cannot resolve %s: host %s: %s", name, res.Host, res.Error)
		}
		var p struct {
			Instances []struct {
				Name           string `json:"name"`
				MigrationPhase string `json:"migration_phase"`
			} `json:"instances"`
		}
		if err := json.Unmarshal(res.Data, &p); err != nil {
			return Host{}, "", err
		}
		for _, v := range p.Instances {
			if v.Name == name && v.MigrationPhase != "completed" {
				h, _ := r.host(res.Host)
				matches = append(matches, h)
			}
		}
	}
	if len(matches) != 1 {
		return Host{}, "", fmt.Errorf("name %q resolves to %d hosts; use host/name", name, len(matches))
	}
	return matches[0], name, nil
}

type placement struct {
	Eligible bool   `json:"eligible"`
	Reason   string `json:"reason"`
	Memory   int64  `json:"memory_left_mib"`
	Disk     int64  `json:"disk_left_bytes"`
}

func (r *Router) choose(ctx context.Context, a model.Actor, args []string) (Host, error) {
	type candidate struct {
		h Host
		p placement
	}
	var candidates []candidate
	var failures []string
	for _, v := range r.all(ctx, a, append([]string{"--json", "placement"}, args...)) {
		var p placement
		if v.Error != "" {
			failures = append(failures, v.Host+": "+v.Error)
			continue
		}
		if err := json.Unmarshal(v.Data, &p); err != nil {
			continue
		}
		if p.Eligible {
			h, _ := r.host(v.Host)
			candidates = append(candidates, candidate{h, p})
		} else {
			failures = append(failures, v.Host+": "+p.Reason)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		x, y := candidates[i], candidates[j]
		if x.p.Memory != y.p.Memory {
			return x.p.Memory > y.p.Memory
		}
		if x.p.Disk != y.p.Disk {
			return x.p.Disk > y.p.Disk
		}
		return x.h.Name < y.h.Name
	})
	if len(candidates) == 0 {
		return Host{}, fmt.Errorf("no eligible host: %s", strings.Join(failures, "; "))
	}
	return candidates[0].h, nil
}

// Run accepts argv, not shell text. --host is a frontend-only option.
func (r *Router) Run(ctx context.Context, a model.Actor, args []string, in io.Reader, out, stderr io.Writer) error {
	var hostName string
	var clean []string
	asJSON := false
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--host":
			i++
			if i == len(args) || args[i] == "" || hostName != "" {
				return fmt.Errorf("--host requires one nonempty value")
			}
			hostName = args[i]
		case strings.HasPrefix(args[i], "--host="):
			if hostName != "" || args[i] == "--host=" {
				return fmt.Errorf("--host requires one nonempty value")
			}
			hostName = strings.TrimPrefix(args[i], "--host=")
		case args[i] == "--json":
			asJSON = true
		default:
			clean = append(clean, args[i])
		}
	}
	args = clean
	if len(args) == 0 {
		return fmt.Errorf("command required")
	}
	cmd := args[0]
	orb := cmd == "orb"
	if orb {
		if len(args) < 2 || (args[1] != "new" && args[1] != "list" && args[1] != "bootstrap") {
			return fmt.Errorf("usage: orb new NAME [options] | orb list | orb bootstrap NAME")
		}
		args = args[1:]
		cmd = args[0]
		if cmd == "bootstrap" && ((len(args) != 2 && len(args) != 8) || asJSON) {
			return fmt.Errorf("usage: orb bootstrap NAME (without --json)")
		}
	}
	if cmd == "help" && hostName == "" {
		if asJSON {
			return fmt.Errorf("help does not support --json")
		}
		_, err := io.WriteString(out, `srv-front: [--json] COMMAND [--host HOST]

node list                            Server CPU, RAM and disk overview
list / status                        Query all hosts
new NAME [sizing/image options]       Automatically choose a host
inspect / start / stop / restart NAME Route to the owning host
resize / fsck / delete NAME           Manage a VM
logs [-f] NAME [serial|firecracker]   Read or follow logs
backup create|list NAME               Manage backups on the owning host
backup delete NAME ID / restore NAME ID
export NAME / import --host HOST      Stream portable VM artifacts
migrate NAME --to HOST                Cold migration (no --json)
pool / integration / snapshot         Require --host
project-template create|delete        Require --host
migration status|receipt NAME         Require --host

Use HOST/NAME when a name is ambiguous or another host is unavailable.
Use direct backend SSH for top/PTY and migration recovery mutations.
`)
		return err
	}
	if cmd == "forward" {
		return fmt.Errorf("reserved command")
	}
	if cmd == "migration" && (len(args) != 3 || (args[1] != "status" && args[1] != "receipt") || hostName == "") {
		return fmt.Errorf("only migration status/receipt NAME --host HOST are supported; recovery mutations require direct backend access")
	}
	if cmd == "top" {
		return fmt.Errorf("top/PTY is not supported by srv-front")
	}
	if ((cmd == "list" || cmd == "status") && hostName == "") || cmd == "node" {
		queryArgs := append([]string{"--json"}, args...)
		if orb {
			queryArgs = append([]string{"--json", "orb"}, args...)
		}
		queryRouter := *r
		if cmd == "node" {
			if len(args) != 2 || args[1] != "list" {
				return fmt.Errorf("usage: node list [--host HOST] [--json]")
			}
			queryArgs = []string{"--json", "status"}
			if hostName != "" {
				h, err := r.host(hostName)
				if err != nil {
					return err
				}
				queryRouter.Hosts = []Host{h}
			}
		}
		results := queryRouter.all(ctx, a, queryArgs)
		var err error
		if asJSON {
			err = json.NewEncoder(out).Encode(map[string]any{"hosts": results})
		} else if cmd == "node" {
			err = renderNodes(out, results)
		} else {
			err = renderAggregate(out, cmd, results)
		}
		for _, v := range results {
			if v.Error != "" {
				fmt.Fprintf(stderr, "%s: %s\n", v.Host, v.Error)
				err = fmt.Errorf("partial response: one or more hosts failed")
			}
		}
		return err
	}
	if cmd == "migrate" {
		if asJSON {
			return fmt.Errorf("migrate does not support --json")
		}
		return r.migrate(ctx, a, args, hostName, out, stderr)
	}
	var h Host
	var err error
	switch cmd {
	case "new":
		for _, arg := range args[1:] {
			if (arg == "--template" || strings.HasPrefix(arg, "--template=")) && hostName == "" {
				return fmt.Errorf("new --template requires --host; templates are host-local")
			}
		}
		if len(args) < 2 || strings.HasPrefix(args[1], "-") {
			return fmt.Errorf("new requires NAME before its options")
		}
		if strings.Contains(args[1], "/") {
			canonical, n, e := r.resolve(ctx, a, args[1])
			if e != nil {
				return e
			}
			h = canonical
			args[1] = n
			if hostName != "" && hostName != h.Name {
				return fmt.Errorf("conflicting host")
			}
		} else if hostName == "" {
			h, err = r.choose(ctx, a, args[1:])
		}
	case "inspect", "start", "stop", "restart", "delete", "resize", "fsck", "restore", "backup", "logs", "export", "bootstrap":
		if cmd == "bootstrap" && !orb {
			return fmt.Errorf("use orb bootstrap NAME")
		}
		nameIndex := 1
		if cmd == "backup" {
			nameIndex = 2
		}
		if cmd == "logs" {
			nameIndex = -1
			for i, arg := range args[1:] {
				if !strings.HasPrefix(arg, "-") && arg != "serial" && arg != "firecracker" {
					nameIndex = i + 1
					break
				}
			}
		}
		if nameIndex < 0 || len(args) <= nameIndex {
			return fmt.Errorf("%s requires name", cmd)
		}
		if hostName == "" || strings.Contains(args[nameIndex], "/") {
			h, args[nameIndex], err = r.resolve(ctx, a, args[nameIndex])
			if hostName != "" && hostName != h.Name {
				return fmt.Errorf("conflicting host")
			}
		}
	case "list", "status", "pool", "integration", "snapshot", "migration", "import", "project-template", "help", "delete-proof", "cleanup-receipt", "operation-result", "operation-cancel":
		if hostName == "" {
			return fmt.Errorf("%s requires --host", cmd)
		}
	default:
		return fmt.Errorf("unsupported command %q", cmd)
	}
	if err != nil {
		return err
	}
	if h.Name == "" {
		h, err = r.host(hostName)
		if err != nil {
			return err
		}
	}
	if orb {
		args = append([]string{"orb"}, args...)
	}
	if asJSON {
		args = append([]string{"--json"}, args...)
	}
	if cmd == "new" {
		fmt.Fprintf(stderr, "host: %s\n", h.Name)
	}
	if cmd != "import" && !(orb && cmd == "bootstrap") {
		in = nil
	}
	if err = r.Transport.Run(ctx, h, a, args, in, out, stderr); err != nil {
		return fmt.Errorf("host %s: outcome may be uncertain; not retried: %w", h.Name, err)
	}
	return nil
}
func (r *Router) migrate(ctx context.Context, a model.Actor, args []string, override string, out, stderr io.Writer) (err error) {
	if len(args) < 4 || args[2] != "--to" {
		return fmt.Errorf("usage: migrate NAME --to HOST [--pool TARGET_POOL | --fixed-memory]")
	}
	policy := args[4:]
	pool := ""
	fixed := false
	if len(policy) == 1 && policy[0] == "--fixed-memory" {
		fixed = true
	} else if len(policy) == 2 && policy[0] == "--pool" && policy[1] != "" && !strings.HasPrefix(policy[1], "--") {
		pool = policy[1]
	} else if len(policy) != 0 {
		return fmt.Errorf("specify either --pool TARGET_POOL or --fixed-memory")
	}
	reference := args[1]
	if override != "" && !strings.Contains(reference, "/") {
		reference = override + "/" + reference
	}
	source, name, err := r.resolve(ctx, a, reference)
	if err != nil {
		return err
	}
	if override != "" && override != source.Name {
		return fmt.Errorf("conflicting source host")
	}
	target, err := r.host(args[3])
	if err != nil {
		return err
	}
	if source.Name == target.Name {
		return fmt.Errorf("source and target are identical")
	}
	data, err := r.query(ctx, source, a, []string{"--json", "inspect", name})
	if err != nil {
		return err
	}
	var inspect struct {
		Instance struct {
			Image string `json:"image"`
			CPUs  int64  `json:"vcpu_count"`
			RAM   int64  `json:"memory_mib"`
			Disk  int64  `json:"rootfs_size_bytes"`
			Pool  string `json:"memory_pool"`
			Mode  string `json:"memory_mode"`
		}
		Integrations []json.RawMessage `json:"integrations"`
	}
	if err = json.Unmarshal(data, &inspect); err != nil {
		return err
	}
	v := inspect.Instance
	if len(inspect.Integrations) > 0 {
		return fmt.Errorf("migration of integrations is not supported")
	}
	if (v.Pool != "" || v.Mode == "pool") && pool == "" && !fixed {
		return fmt.Errorf("pooled source requires --pool TARGET_POOL or --fixed-memory")
	}
	if v.Image == "" || v.CPUs <= 0 || v.RAM <= 0 || v.Disk <= 0 {
		return fmt.Errorf("source inspect lacks migration shape")
	}
	preflight := []string{"--json", "import-placement", name, "--image", v.Image, "--cpus", fmt.Sprint(v.CPUs), "--ram", fmt.Sprintf("%dMiB", v.RAM), "--rootfs-size", fmt.Sprintf("%dB", v.Disk)}
	if pool != "" {
		preflight = append(preflight, "--pool", pool)
	}
	data, err = r.query(ctx, target, a, preflight)
	if err != nil {
		return err
	}
	var p placement
	if err = json.Unmarshal(data, &p); err != nil {
		return err
	}
	if !p.Eligible {
		return fmt.Errorf("target ineligible: %s", p.Reason)
	}
	token := uuid.NewString()
	defer func() {
		if err != nil {
			err = fmt.Errorf("migration source=%s target=%s name=%s token=%s: %w; do not retry automatically; inspect migration status on source and receipt on target and recover manually; do not start source while target may exist", source.Name, target.Name, name, token, err)
		}
	}()
	fmt.Fprintf(stderr, "migration %s/%s -> %s token=%s: prepare\n", source.Name, name, target.Name, token)
	data, err = r.commandJSON(ctx, source, a, append([]string{"--json", "migration", "prepare", name, target.Name, token}, policy...))
	if err != nil {
		return err
	}
	var prepared struct {
		InstanceID string `json:"instance_id"`
		Token      string `json:"token"`
		Target     string `json:"target"`
		WasRunning bool   `json:"was_running"`
		Phase      string `json:"phase"`
	}
	if err = json.Unmarshal(data, &prepared); err != nil {
		return err
	}
	if prepared.Token != token || prepared.Target != target.Name || prepared.Phase != "prepared" {
		return fmt.Errorf("prepare response mismatch")
	}
	fmt.Fprintln(stderr, "migration: streaming export to import")
	// Both SSH sessions can emit progress/errors concurrently.
	streamErrors := &lockedWriter{writer: stderr}
	pipeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	rd, wr := io.Pipe()
	defer rd.Close()
	defer wr.Close()
	done := make(chan error, 1)
	go func() {
		e := r.Transport.Run(pipeCtx, source, a, []string{"export", name}, nil, wr, streamErrors)
		wr.CloseWithError(e)
		if e != nil {
			cancel()
		}
		done <- e
	}()
	err = r.Transport.Run(pipeCtx, target, a, append([]string{"import"}, policy...), rd, io.Discard, streamErrors)
	rd.CloseWithError(err)
	if err != nil {
		cancel()
	}
	exportErr := <-done
	if err != nil {
		return err
	}
	if exportErr != nil {
		return exportErr
	}
	data, err = r.query(ctx, target, a, []string{"--json", "migration", "receipt", name})
	if err != nil {
		return err
	}
	var receipt struct {
		Token            string `json:"token"`
		SourceInstanceID string `json:"source_instance_id"`
		Phase            string `json:"phase"`
	}
	if err = json.Unmarshal(data, &receipt); err != nil {
		return err
	}
	if prepared.InstanceID == "" || receipt.SourceInstanceID != prepared.InstanceID || receipt.Token != prepared.Token || receipt.Phase != "imported" {
		return fmt.Errorf("target import receipt mismatch")
	}
	fmt.Fprintln(stderr, "migration: completing source")
	_, err = r.commandJSON(ctx, source, a, []string{"--json", "migration", "complete", name, token})
	if err != nil {
		return err
	}
	if prepared.WasRunning {
		err = r.Transport.Run(ctx, target, a, []string{"start", name}, nil, out, stderr)
		if err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "migrated: %s/%s\n", target.Name, name)
	return nil
}

type lockedWriter struct {
	mu     sync.Mutex
	writer io.Writer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writer.Write(p)
}

func renderAggregate(out io.Writer, cmd string, results []Result) error {
	if cmd == "status" {
		for _, res := range results {
			fmt.Fprintf(out, "Host %s:\n", res.Host)
			if res.Error != "" {
				fmt.Fprintf(out, "  ERROR: %s\n", res.Error)
				continue
			}
			var summary host.CapacitySummary
			if err := json.Unmarshal(res.Data, &summary); err != nil {
				return err
			}
			fmt.Fprintf(out, "  %s (%s), %d running / %d instances\n", summary.Hostname, summary.Version, summary.Instances.Running, summary.Instances.Total)
			w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "  RESOURCE\tALLOCATED\tBUDGET\tLEFT\tNOTE")
			for _, resource := range summary.Capacity {
				values := []string{fmt.Sprint(resource.Allocated), fmt.Sprint(resource.Budget), fmt.Sprint(resource.Left)}
				if resource.Unit == "bytes" {
					values = []string{format.BinarySize(resource.Allocated), format.BinarySize(resource.Budget), format.BinarySize(resource.Left)}
				}
				fmt.Fprintf(w, "  %s\t%s\t%s\t%s\t%s\n", resource.Resource, values[0], values[1], values[2], resource.Note)
			}
			if err := w.Flush(); err != nil {
				return err
			}
		}
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "HOST\tNAME\tKIND\tSTATE\tCPU\tRAM\tDISK\tMIGRATION")
	for _, res := range results {
		if res.Error != "" {
			fmt.Fprintf(w, "%s\tERROR: %s\n", res.Host, res.Error)
			continue
		}
		var p struct {
			Instances []struct {
				Name            string `json:"name"`
				Kind            string `json:"kind"`
				State           string `json:"state"`
				CPU             int64  `json:"vcpu_count"`
				RAM             int64  `json:"memory_mib"`
				Disk            int64  `json:"rootfs_size_bytes"`
				MigrationPhase  string `json:"migration_phase"`
				MigrationTarget string `json:"migration_target"`
			} `json:"instances"`
		}
		if err := json.Unmarshal(res.Data, &p); err != nil {
			return err
		}
		for _, v := range p.Instances {
			migration := ""
			if v.MigrationPhase != "" {
				migration = v.MigrationPhase + " -> " + v.MigrationTarget
			}
			kind := (model.Instance{Kind: v.Kind}).KindName()
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\n", res.Host, v.Name, kind, v.State, v.CPU, format.BinarySize(v.RAM*format.MiB), format.BinarySize(v.Disk), migration)
		}
	}
	return w.Flush()
}

func renderNodes(out io.Writer, results []Result) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "HOST\tSTATE\tINSTANCES (running/total)\tCPU (vCPU/budget)\tLOAD (1m)\tRAM (reserved/budget)\tDISK (reserved/budget)\tDISK AVAILABLE")
	for _, res := range results {
		if res.Error != "" {
			fmt.Fprintf(w, "%s\tERROR: %s\t-\t-\t-\t-\t-\t-\n", res.Host, res.Error)
			continue
		}
		var summary host.CapacitySummary
		if err := json.Unmarshal(res.Data, &summary); err != nil {
			return err
		}
		cpu, memory, disk, available := "-", "-", "-", "-"
		for _, resource := range summary.Capacity {
			switch resource.Resource {
			case "cpu":
				cpu = fmt.Sprintf("%d / %d", resource.Allocated, resource.Budget)
			case "memory":
				memory = format.BinarySize(resource.Allocated) + " / " + format.BinarySize(resource.Budget)
			case "disk":
				disk = format.BinarySize(resource.Allocated) + " / " + format.BinarySize(resource.Budget)
				if resource.Filesystem != nil {
					available = format.BinarySize(resource.Filesystem.Available)
				}
			}
		}
		fmt.Fprintf(w, "%s\tok\t%d / %d\t%s\t%.2f\t%s\t%s\t%s\n", res.Host, summary.Instances.Running, summary.Instances.Total, cpu, summary.CPU.Load1m, memory, disk, available)
	}
	return w.Flush()
}
