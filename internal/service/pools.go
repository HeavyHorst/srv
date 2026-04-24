package service

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"srv/internal/format"
	"srv/internal/model"
)

type memoryPoolSummaryJSON struct {
	Name          string    `json:"name"`
	ReservedBytes int64     `json:"reserved_bytes"`
	MemberCount   int       `json:"member_count"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type memoryPoolActionJSON struct {
	Action string                `json:"action"`
	Pool   memoryPoolSummaryJSON `json:"pool"`
}

type memoryPoolListJSON struct {
	Pools []memoryPoolSummaryJSON `json:"pools"`
}

type memoryPoolInspectJSON struct {
	Pool    memoryPoolSummaryJSON `json:"pool"`
	Members []instanceSummaryJSON `json:"members"`
}

func (a *App) cmdPool(ctx context.Context, actor model.Actor, args []string, outFormat outputFormat) (commandResult, error) {
	if len(args) < 2 {
		err := errors.New(poolUsage())
		return commandResult{stderr: err.Error() + "\n", exitCode: 2}, err
	}
	if result, err, rejected := maybeUnsupportedJSONCommand(args[0], outFormat); rejected {
		return result, err
	}
	switch args[1] {
	case "create":
		return a.cmdPoolCreate(ctx, actor, args, outFormat)
	case "list":
		return a.cmdPoolList(ctx, args, outFormat)
	case "inspect":
		return a.cmdPoolInspect(ctx, args, outFormat)
	case "delete":
		return a.cmdPoolDelete(ctx, args, outFormat)
	default:
		err := fmt.Errorf("unknown pool action %q\n%s", args[1], poolUsage())
		return commandResult{stderr: err.Error() + "\n", exitCode: 2}, err
	}
}

func (a *App) cmdPoolCreate(ctx context.Context, actor model.Actor, args []string, outFormat outputFormat) (commandResult, error) {
	name, sizeBytes, err := parsePoolCreateArgs(args)
	if err != nil {
		return commandResult{stderr: err.Error() + "\n", exitCode: 2}, err
	}

	pool, err := a.provisioner.CreateMemoryPool(ctx, name, actor, sizeBytes)
	if err != nil {
		return commandResult{stderr: fmt.Sprintf("pool create %s: %v\n", name, err), exitCode: 1}, err
	}
	members, err := a.store.CountMemoryPoolMembers(ctx, pool.ID)
	if err != nil {
		return commandResult{stderr: fmt.Sprintf("count pool members: %v\n", err), exitCode: 1}, err
	}
	payload := memoryPoolSummaryPayload(pool, members)
	if outFormat == outputFormatJSON {
		return jsonResult(memoryPoolActionJSON{Action: "pool-created", Pool: payload})
	}
	stdout := fmt.Sprintf("pool-created: %s\nreserved: %s\nmembers: %d\n", pool.Name, format.BinarySize(pool.ReservedBytes), members)
	return commandResult{stdout: stdout, exitCode: 0}, nil
}

func (a *App) cmdPoolList(ctx context.Context, args []string, outFormat outputFormat) (commandResult, error) {
	if len(args) != 2 {
		err := errors.New(poolListUsage())
		return commandResult{stderr: err.Error() + "\n", exitCode: 2}, err
	}
	pools, err := a.store.ListMemoryPools(ctx)
	if err != nil {
		return commandResult{stderr: fmt.Sprintf("pool list: %v\n", err), exitCode: 1}, err
	}
	payload := memoryPoolListJSON{Pools: make([]memoryPoolSummaryJSON, 0, len(pools))}
	rows := make([][]string, 0, len(pools))
	for _, pool := range pools {
		members, err := a.store.CountMemoryPoolMembers(ctx, pool.ID)
		if err != nil {
			return commandResult{stderr: fmt.Sprintf("count pool members: %v\n", err), exitCode: 1}, err
		}
		summary := memoryPoolSummaryPayload(pool, members)
		payload.Pools = append(payload.Pools, summary)
		rows = append(rows, []string{
			pool.Name,
			format.BinarySize(pool.ReservedBytes),
			strconv.Itoa(members),
			pool.CreatedAt.Format(time.RFC3339),
		})
	}
	if outFormat == outputFormatJSON {
		return jsonResult(payload)
	}
	if len(rows) == 0 {
		return commandResult{stdout: "no memory pools\n", exitCode: 0}, nil
	}
	tableOutput, err := renderTextTable([]string{"Name", "Reserved", "Members", "Created At"}, rows)
	if err != nil {
		return commandResult{stderr: fmt.Sprintf("render pool list: %v\n", err), exitCode: 1}, err
	}
	return commandResult{stdout: tableOutput, exitCode: 0}, nil
}

func (a *App) cmdPoolInspect(ctx context.Context, args []string, outFormat outputFormat) (commandResult, error) {
	if len(args) != 3 {
		err := errors.New(poolInspectUsage())
		return commandResult{stderr: err.Error() + "\n", exitCode: 2}, err
	}
	pool, err := a.store.GetMemoryPoolByName(ctx, args[2])
	if err != nil {
		return commandResult{stderr: fmt.Sprintf("pool inspect %s: %v\n", args[2], err), exitCode: 1}, err
	}
	instances, err := a.store.ListInstances(ctx, false)
	if err != nil {
		return commandResult{stderr: fmt.Sprintf("list instances: %v\n", err), exitCode: 1}, err
	}
	members := make([]model.Instance, 0)
	for _, inst := range instances {
		if inst.MemoryPoolID == pool.ID {
			members = append(members, inst)
		}
	}
	payload := memoryPoolInspectJSON{
		Pool:    memoryPoolSummaryPayload(pool, len(members)),
		Members: make([]instanceSummaryJSON, 0, len(members)),
	}
	for _, inst := range members {
		payload.Members = append(payload.Members, instanceSummaryPayload(a.cfg, inst, false))
	}
	if outFormat == outputFormatJSON {
		return jsonResult(payload)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "name: %s\n", pool.Name)
	fmt.Fprintf(&b, "reserved: %s\n", format.BinarySize(pool.ReservedBytes))
	fmt.Fprintf(&b, "members: %d\n", len(members))
	fmt.Fprintf(&b, "created-at: %s\n", pool.CreatedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "updated-at: %s\n", pool.UpdatedAt.Format(time.RFC3339))
	if len(members) == 0 {
		b.WriteString("member-vms: none\n")
	} else {
		b.WriteString("member-vms:\n")
		for _, inst := range members {
			fmt.Fprintf(&b, "- %s (%s guest RAM, state: %s)\n", inst.Name, format.BinarySize(effectiveInstanceMemoryMiB(inst, a.cfg)*mib), inst.State)
		}
	}
	return commandResult{stdout: b.String(), exitCode: 0}, nil
}

func (a *App) cmdPoolDelete(ctx context.Context, args []string, outFormat outputFormat) (commandResult, error) {
	if len(args) != 3 {
		err := errors.New(poolDeleteUsage())
		return commandResult{stderr: err.Error() + "\n", exitCode: 2}, err
	}
	pool, err := a.provisioner.DeleteMemoryPool(ctx, args[2])
	if err != nil {
		return commandResult{stderr: fmt.Sprintf("pool delete %s: %v\n", args[2], err), exitCode: 1}, err
	}
	payload := memoryPoolSummaryPayload(pool, 0)
	if outFormat == outputFormatJSON {
		return jsonResult(memoryPoolActionJSON{Action: "pool-deleted", Pool: payload})
	}
	return commandResult{stdout: fmt.Sprintf("pool-deleted: %s\n", pool.Name), exitCode: 0}, nil
}

func memoryPoolSummaryPayload(pool model.MemoryPool, members int) memoryPoolSummaryJSON {
	return memoryPoolSummaryJSON{
		Name:          pool.Name,
		ReservedBytes: pool.ReservedBytes,
		MemberCount:   members,
		CreatedAt:     pool.CreatedAt,
		UpdatedAt:     pool.UpdatedAt,
	}
}

func parsePoolCreateArgs(args []string) (string, int64, error) {
	if len(args) < 4 {
		return "", 0, errors.New(poolCreateUsage())
	}
	var (
		name     string
		size     int64
		seenSize bool
	)
	for i := 2; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "--") {
			if name != "" {
				return "", 0, fmt.Errorf("unexpected argument %q\n%s", arg, poolCreateUsage())
			}
			name = arg
			continue
		}
		key, value, hasValue := strings.Cut(arg, "=")
		if !hasValue {
			i++
			if i >= len(args) {
				return "", 0, fmt.Errorf("missing value for %s\n%s", key, poolCreateUsage())
			}
			value = args[i]
		}
		switch key {
		case "--size":
			if seenSize {
				return "", 0, fmt.Errorf("%s specified more than once\n%s", key, poolCreateUsage())
			}
			seenSize = true
			parsed, err := parseSize(value, mib)
			if err != nil {
				return "", 0, fmt.Errorf("parse %s: %v\n%s", key, err, poolCreateUsage())
			}
			size = parsed
		default:
			return "", 0, fmt.Errorf("unknown option %q\n%s", key, poolCreateUsage())
		}
	}
	if name == "" || !seenSize {
		return "", 0, errors.New(poolCreateUsage())
	}
	return name, size, nil
}

func poolUsage() string {
	return "usage: pool <create|list|inspect|delete> ..."
}

func poolCreateUsage() string {
	return "usage: pool create <name> --size SIZE"
}

func poolListUsage() string {
	return "usage: pool list"
}

func poolInspectUsage() string {
	return "usage: pool inspect <name>"
}

func poolDeleteUsage() string {
	return "usage: pool delete <name>"
}
