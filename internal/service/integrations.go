package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"srv/internal/model"
	"srv/internal/provision"
)

const integrationSecretEnvPrefix = "SRV_SECRET_"

var validIntegrationName = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
var validIntegrationHeaderName = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

type integrationSummaryJSON struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Target   string `json:"target"`
	AuthMode string `json:"auth_mode"`
}

type integrationListResponseJSON struct {
	Integrations []integrationSummaryJSON `json:"integrations"`
}

type integrationInspectResponseJSON struct {
	Integration integrationInspectJSON `json:"integration"`
}

type integrationInspectJSON struct {
	Name             string                    `json:"name"`
	Type             string                    `json:"type"`
	Target           string                    `json:"target"`
	AuthMode         string                    `json:"auth_mode"`
	BearerEnv        string                    `json:"bearer_env,omitempty"`
	BasicUser        string                    `json:"basic_user,omitempty"`
	BasicPasswordEnv string                    `json:"basic_password_env,omitempty"`
	Headers          []model.IntegrationHeader `json:"headers,omitempty"`
	CreatedAt        time.Time                 `json:"created_at"`
	UpdatedAt        time.Time                 `json:"updated_at"`
}

type integrationActionJSON struct {
	Action      string                 `json:"action"`
	Integration integrationSummaryJSON `json:"integration"`
}

type instanceIntegrationListResponseJSON struct {
	Instance     string                   `json:"instance"`
	Integrations []inspectIntegrationJSON `json:"integrations"`
}

type integrationAddOptions struct {
	TargetURL        string
	AuthMode         model.IntegrationAuthMode
	BearerEnv        string
	BasicUser        string
	BasicPasswordEnv string
	Headers          []model.IntegrationHeader
}

func (a *App) cmdIntegration(ctx context.Context, actor model.Actor, args []string, outFormat outputFormat) (commandResult, error) {
	if len(args) < 2 {
		err := errors.New(integrationUsage())
		return commandResult{stderr: err.Error() + "\n", exitCode: 2}, err
	}

	switch args[1] {
	case "list":
		if len(args) != 2 {
			err := errors.New(integrationListUsage())
			return commandResult{stderr: err.Error() + "\n", exitCode: 2}, err
		}
		integrations, err := a.store.ListIntegrations(ctx)
		if err != nil {
			return commandResult{stderr: fmt.Sprintf("list integrations: %v\n", err), exitCode: 1}, err
		}
		if outFormat == outputFormatJSON {
			payload := integrationListResponseJSON{Integrations: make([]integrationSummaryJSON, 0, len(integrations))}
			for _, integration := range integrations {
				payload.Integrations = append(payload.Integrations, integrationSummaryPayload(integration))
			}
			return jsonResult(payload)
		}
		if len(integrations) == 0 {
			return commandResult{stdout: "no integrations\n", exitCode: 0}, nil
		}
		rows := make([][]string, 0, len(integrations))
		for _, integration := range integrations {
			rows = append(rows, []string{integration.Name, string(integration.Kind), integration.TargetURL, string(integration.AuthMode)})
		}
		tableOutput, err := renderTextTable([]string{"Name", "Type", "Target", "Auth Mode"}, rows)
		if err != nil {
			return commandResult{stderr: fmt.Sprintf("render integrations: %v\n", err), exitCode: 1}, err
		}
		return commandResult{stdout: tableOutput, exitCode: 0}, nil
	case "inspect":
		name, err := parseIntegrationNameAction(args, integrationInspectUsage())
		if err != nil {
			return commandResult{stderr: err.Error() + "\n", exitCode: 2}, err
		}
		integration, err := a.store.GetIntegrationByName(ctx, name)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				err = fmt.Errorf("integration %q does not exist", name)
				return commandResult{stderr: err.Error() + "\n", exitCode: 1}, err
			}
			return commandResult{stderr: fmt.Sprintf("inspect integration %s: %v\n", name, err), exitCode: 1}, err
		}
		payload, err := integrationInspectPayload(integration)
		if err != nil {
			return commandResult{stderr: fmt.Sprintf("inspect integration %s: %v\n", name, err), exitCode: 1}, err
		}
		if outFormat == outputFormatJSON {
			return jsonResult(integrationInspectResponseJSON{Integration: payload})
		}
		var b strings.Builder
		fmt.Fprintf(&b, "name: %s\n", payload.Name)
		fmt.Fprintf(&b, "type: %s\n", payload.Type)
		fmt.Fprintf(&b, "target: %s\n", payload.Target)
		fmt.Fprintf(&b, "auth-mode: %s\n", payload.AuthMode)
		if payload.BearerEnv != "" {
			fmt.Fprintf(&b, "bearer-env: %s\n", payload.BearerEnv)
		}
		if payload.BasicUser != "" {
			fmt.Fprintf(&b, "basic-user: %s\n", payload.BasicUser)
		}
		if payload.BasicPasswordEnv != "" {
			fmt.Fprintf(&b, "basic-password-env: %s\n", payload.BasicPasswordEnv)
		}
		if len(payload.Headers) > 0 {
			b.WriteString("headers:\n")
			for _, header := range payload.Headers {
				if header.Env != "" {
					fmt.Fprintf(&b, "- %s: $%s\n", header.Name, header.Env)
					continue
				}
				fmt.Fprintf(&b, "- %s: %s\n", header.Name, header.Value)
			}
		}
		fmt.Fprintf(&b, "created-at: %s\n", payload.CreatedAt.Format(time.RFC3339))
		fmt.Fprintf(&b, "updated-at: %s\n", payload.UpdatedAt.Format(time.RFC3339))
		return commandResult{stdout: b.String(), exitCode: 0}, nil
	case "add":
		kind, name, opts, err := parseIntegrationAddArgs(args)
		if err != nil {
			return commandResult{stderr: err.Error() + "\n", exitCode: 2}, err
		}
		if kind != string(model.IntegrationKindHTTP) {
			err := fmt.Errorf("unsupported integration type %q\n%s", kind, integrationAddUsage())
			return commandResult{stderr: err.Error() + "\n", exitCode: 2}, err
		}
		headersJSON, err := json.Marshal(opts.Headers)
		if err != nil {
			return commandResult{stderr: fmt.Sprintf("marshal headers: %v\n", err), exitCode: 1}, err
		}
		now := time.Now().UTC()
		integration := model.Integration{
			ID:               uuid.NewString(),
			Name:             name,
			Kind:             model.IntegrationKindHTTP,
			TargetURL:        opts.TargetURL,
			AuthMode:         opts.AuthMode,
			BearerEnv:        opts.BearerEnv,
			BasicUser:        opts.BasicUser,
			BasicPasswordEnv: opts.BasicPasswordEnv,
			HeadersJSON:      string(headersJSON),
			CreatedAt:        now,
			UpdatedAt:        now,
		}
		if err := a.store.CreateIntegration(ctx, integration); err != nil {
			return commandResult{stderr: fmt.Sprintf("create integration %s: %v\n", name, err), exitCode: 1}, err
		}
		if outFormat == outputFormatJSON {
			return jsonResult(integrationActionJSON{Action: "integration-created", Integration: integrationSummaryPayload(integration)})
		}
		return commandResult{stdout: fmt.Sprintf("integration-created: %s\ntype: %s\ntarget: %s\nauth-mode: %s\n", integration.Name, integration.Kind, integration.TargetURL, integration.AuthMode), exitCode: 0}, nil
	case "delete":
		name, err := parseIntegrationNameAction(args, integrationDeleteUsage())
		if err != nil {
			return commandResult{stderr: err.Error() + "\n", exitCode: 2}, err
		}
		integration, err := a.store.GetIntegrationByName(ctx, name)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				err = fmt.Errorf("integration %q does not exist", name)
				return commandResult{stderr: err.Error() + "\n", exitCode: 1}, err
			}
			return commandResult{stderr: fmt.Sprintf("delete integration %s: %v\n", name, err), exitCode: 1}, err
		}
		count, err := a.store.CountIntegrationBindings(ctx, integration.ID)
		if err != nil {
			return commandResult{stderr: fmt.Sprintf("delete integration %s: %v\n", name, err), exitCode: 1}, err
		}
		if count > 0 {
			err := fmt.Errorf("integration %q is still enabled on %d VM(s)", name, count)
			return commandResult{stderr: err.Error() + "\n", exitCode: 1}, err
		}
		if err := a.store.DeleteIntegration(ctx, name); err != nil {
			return commandResult{stderr: fmt.Sprintf("delete integration %s: %v\n", name, err), exitCode: 1}, err
		}
		if outFormat == outputFormatJSON {
			return jsonResult(map[string]string{"action": "integration-deleted", "name": name})
		}
		return commandResult{stdout: fmt.Sprintf("integration-deleted: %s\n", name), exitCode: 0}, nil
	case "enable":
		vmName, integrationName, err := parseIntegrationBindingArgs(args, integrationEnableUsage())
		if err != nil {
			return commandResult{stderr: err.Error() + "\n", exitCode: 2}, err
		}
		inst, err := a.lookupVisibleInstance(ctx, actor, vmName)
		if err != nil {
			return missingInstanceResult("integration enable", vmName, err)
		}
		integration, err := a.store.GetIntegrationByName(ctx, integrationName)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				err = fmt.Errorf("integration %q does not exist", integrationName)
				return commandResult{stderr: err.Error() + "\n", exitCode: 1}, err
			}
			return commandResult{stderr: fmt.Sprintf("enable integration %s on %s: %v\n", integrationName, vmName, err), exitCode: 1}, err
		}
		if err := a.store.BindIntegrationToInstance(ctx, model.InstanceIntegrationBinding{
			InstanceID:    inst.ID,
			IntegrationID: integration.ID,
			CreatedAt:     time.Now().UTC(),
			CreatedByUser: actor.UserLogin,
			CreatedByNode: actor.NodeName,
		}); err != nil {
			return commandResult{stderr: fmt.Sprintf("enable integration %s on %s: %v\n", integrationName, vmName, err), exitCode: 1}, err
		}
		a.syncManagedGatewaysBestEffort()
		url := a.integrationGatewayBaseURL(inst, integration.Name)
		if outFormat == outputFormatJSON {
			return jsonResult(map[string]string{"action": "integration-enabled", "instance": vmName, "name": integration.Name, "url": url})
		}
		stdout := fmt.Sprintf("integration-enabled: %s\ninstance: %s\n", integration.Name, vmName)
		if url != "" {
			stdout += fmt.Sprintf("url: %s\n", url)
		}
		return commandResult{stdout: stdout, exitCode: 0}, nil
	case "disable":
		vmName, integrationName, err := parseIntegrationBindingArgs(args, integrationDisableUsage())
		if err != nil {
			return commandResult{stderr: err.Error() + "\n", exitCode: 2}, err
		}
		inst, err := a.lookupVisibleInstance(ctx, actor, vmName)
		if err != nil {
			return missingInstanceResult("integration disable", vmName, err)
		}
		integration, err := a.store.GetIntegrationByName(ctx, integrationName)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				err = fmt.Errorf("integration %q does not exist", integrationName)
				return commandResult{stderr: err.Error() + "\n", exitCode: 1}, err
			}
			return commandResult{stderr: fmt.Sprintf("disable integration %s on %s: %v\n", integrationName, vmName, err), exitCode: 1}, err
		}
		if err := a.store.UnbindIntegrationFromInstance(ctx, inst.ID, integration.ID); err != nil {
			return commandResult{stderr: fmt.Sprintf("disable integration %s on %s: %v\n", integrationName, vmName, err), exitCode: 1}, err
		}
		a.syncManagedGatewaysBestEffort()
		if outFormat == outputFormatJSON {
			return jsonResult(map[string]string{"action": "integration-disabled", "instance": vmName, "name": integration.Name})
		}
		return commandResult{stdout: fmt.Sprintf("integration-disabled: %s\ninstance: %s\n", integration.Name, vmName), exitCode: 0}, nil
	case "list-enabled":
		vmName, err := parseIntegrationListEnabledArgs(args)
		if err != nil {
			return commandResult{stderr: err.Error() + "\n", exitCode: 2}, err
		}
		inst, err := a.lookupVisibleInstance(ctx, actor, vmName)
		if err != nil {
			return missingInstanceResult("integration list-enabled", vmName, err)
		}
		integrations, err := a.inspectIntegrationsForInstance(ctx, inst)
		if err != nil {
			return commandResult{stderr: fmt.Sprintf("list enabled integrations for %s: %v\n", vmName, err), exitCode: 1}, err
		}
		if outFormat == outputFormatJSON {
			return jsonResult(instanceIntegrationListResponseJSON{Instance: vmName, Integrations: integrations})
		}
		if len(integrations) == 0 {
			return commandResult{stdout: fmt.Sprintf("no integrations enabled for %s\n", vmName), exitCode: 0}, nil
		}
		rows := make([][]string, 0, len(integrations))
		for _, integration := range integrations {
			rows = append(rows, []string{integration.Name, integration.Type, integration.URL})
		}
		tableOutput, err := renderTextTable([]string{"Name", "Type", "URL"}, rows)
		if err != nil {
			return commandResult{stderr: fmt.Sprintf("render enabled integrations: %v\n", err), exitCode: 1}, err
		}
		return commandResult{stdout: tableOutput, exitCode: 0}, nil
	default:
		err := fmt.Errorf("unknown integration action %q\n%s", args[1], integrationUsage())
		return commandResult{stderr: err.Error() + "\n", exitCode: 2}, err
	}
}

func integrationSummaryPayload(integration model.Integration) integrationSummaryJSON {
	return integrationSummaryJSON{
		Name:     integration.Name,
		Type:     string(integration.Kind),
		Target:   integration.TargetURL,
		AuthMode: string(integration.AuthMode),
	}
}

func integrationInspectPayload(integration model.Integration) (integrationInspectJSON, error) {
	headers, err := decodeIntegrationHeaders(integration.HeadersJSON)
	if err != nil {
		return integrationInspectJSON{}, err
	}
	return integrationInspectJSON{
		Name:             integration.Name,
		Type:             string(integration.Kind),
		Target:           integration.TargetURL,
		AuthMode:         string(integration.AuthMode),
		BearerEnv:        integration.BearerEnv,
		BasicUser:        integration.BasicUser,
		BasicPasswordEnv: integration.BasicPasswordEnv,
		Headers:          headers,
		CreatedAt:        integration.CreatedAt,
		UpdatedAt:        integration.UpdatedAt,
	}, nil
}

func decodeIntegrationHeaders(raw string) ([]model.IntegrationHeader, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var headers []model.IntegrationHeader
	if err := json.Unmarshal([]byte(raw), &headers); err != nil {
		return nil, fmt.Errorf("decode integration headers: %w", err)
	}
	return headers, nil
}

func (a *App) inspectIntegrationsForInstance(ctx context.Context, inst model.Instance) ([]inspectIntegrationJSON, error) {
	integrations, err := a.store.ListInstanceIntegrations(ctx, inst.ID)
	if err != nil {
		return nil, err
	}
	if len(integrations) == 0 {
		return nil, nil
	}
	visible := make([]inspectIntegrationJSON, 0, len(integrations))
	for _, integration := range integrations {
		gatewayURL := a.integrationGatewayBaseURL(inst, integration.Name)
		visible = append(visible, inspectIntegrationJSON{Name: integration.Name, Type: string(integration.Kind), URL: gatewayURL})
	}
	sort.Slice(visible, func(i, j int) bool {
		return visible[i].Name < visible[j].Name
	})
	return visible, nil
}

func (a *App) integrationGatewayBaseURL(inst model.Instance, name string) string {
	if a == nil || a.integrationGateway == nil || !shouldExposeGateway(inst) {
		return ""
	}
	hostIP, ok := stripInstanceIP(inst.HostAddr)
	if !ok {
		return ""
	}
	return fmt.Sprintf("http://%s:%d/integrations/%s", hostIP, a.cfg.IntegrationGatewayPort, name)
}

func (a *App) syncManagedGatewaysBestEffort() {
	a.syncZenGatewayBestEffort()
	a.syncDeepseekGatewayBestEffort()
	a.syncIntegrationGatewayBestEffort()
}

func (a *App) syncIntegrationGatewayBestEffort() {
	if a == nil || a.integrationGateway == nil || a.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	instances, err := a.store.ListInstances(ctx, false)
	if err != nil {
		a.log.Error("list instances for integration gateway sync", "err", err)
		return
	}
	specs := make(map[string]integrationGatewaySpec)
	for _, inst := range instances {
		if !shouldExposeGateway(inst) {
			continue
		}
		hostIP, ok := stripInstanceIP(inst.HostAddr)
		if !ok {
			continue
		}
		guestIP, ok := stripInstanceIP(inst.GuestAddr)
		if !ok {
			continue
		}
		integrations, err := a.store.ListInstanceIntegrations(ctx, inst.ID)
		if err != nil {
			a.log.Error("list integrations for instance", "instance", inst.Name, "err", err)
			continue
		}
		if len(integrations) == 0 {
			continue
		}
		routes := make(map[string]integrationRouteSpec, len(integrations))
		for _, integration := range integrations {
			headers, err := decodeIntegrationHeaders(integration.HeadersJSON)
			if err != nil {
				a.log.Error("decode integration headers", "integration", integration.Name, "err", err)
				continue
			}
			parsedTarget, err := url.Parse(integration.TargetURL)
			if err != nil {
				a.log.Error("parse integration target", "integration", integration.Name, "err", err)
				continue
			}
			routes[integration.Name] = integrationRouteSpec{
				Name:             integration.Name,
				TargetURL:        parsedTarget.String(),
				AuthMode:         integration.AuthMode,
				BearerEnv:        integration.BearerEnv,
				BasicUser:        integration.BasicUser,
				BasicPasswordEnv: integration.BasicPasswordEnv,
				Headers:          headers,
			}
		}
		if len(routes) == 0 {
			continue
		}
		specs[inst.Name] = integrationGatewaySpec{Name: inst.Name, HostIP: hostIP, GuestIP: guestIP, Routes: routes}
	}
	if err := a.integrationGateway.Reconcile(ctx, specs); err != nil {
		a.log.Error("sync integration gateways", "err", err)
	}
}

func (a *App) lookupIntegrationsByName(ctx context.Context, names []string) ([]model.Integration, error) {
	if len(names) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(names))
	integrations := make([]model.Integration, 0, len(names))
	for _, name := range names {
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		integration, err := a.store.GetIntegrationByName(ctx, name)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil, fmt.Errorf("integration %q does not exist", name)
			}
			return nil, fmt.Errorf("lookup integration %s: %w", name, err)
		}
		integrations = append(integrations, integration)
	}
	return integrations, nil
}

func parseNewArgs(args []string) (string, provision.CreateOptions, []string, error) {
	if len(args) < 2 {
		return "", provision.CreateOptions{}, nil, errors.New(newUsage())
	}

	var (
		name           string
		opts           provision.CreateOptions
		integrations   []string
		seenCPUs       bool
		seenRAM        bool
		seenPool       bool
		seenRootFSSize bool
	)

	for i := 1; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "--") {
			if name != "" {
				return "", provision.CreateOptions{}, nil, fmt.Errorf("unexpected argument %q\n%s", arg, newUsage())
			}
			name = arg
			continue
		}

		key, value, hasValue := strings.Cut(arg, "=")
		if !hasValue {
			i++
			if i >= len(args) {
				return "", provision.CreateOptions{}, nil, fmt.Errorf("missing value for %s\n%s", key, newUsage())
			}
			value = args[i]
		}

		switch key {
		case "--cpus":
			if seenCPUs {
				return "", provision.CreateOptions{}, nil, fmt.Errorf("%s specified more than once\n%s", key, newUsage())
			}
			seenCPUs = true
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return "", provision.CreateOptions{}, nil, fmt.Errorf("parse %s: %w\n%s", key, err, newUsage())
			}
			opts.VCPUCount = parsed
		case "--ram":
			if seenRAM {
				return "", provision.CreateOptions{}, nil, fmt.Errorf("%s specified more than once\n%s", key, newUsage())
			}
			seenRAM = true
			parsed, err := parseSize(value, mib)
			if err != nil {
				return "", provision.CreateOptions{}, nil, fmt.Errorf("parse %s: %v\n%s", key, err, newUsage())
			}
			opts.MemoryMiB = bytesToMiBCeil(parsed)
		case "--pool":
			if seenPool {
				return "", provision.CreateOptions{}, nil, fmt.Errorf("%s specified more than once\n%s", key, newUsage())
			}
			seenPool = true
			poolName := strings.TrimSpace(value)
			if !validIntegrationName.MatchString(poolName) {
				return "", provision.CreateOptions{}, nil, fmt.Errorf("invalid memory pool name %q\n%s", poolName, newUsage())
			}
			opts.MemoryPoolName = poolName
		case "--rootfs-size":
			if seenRootFSSize {
				return "", provision.CreateOptions{}, nil, fmt.Errorf("%s specified more than once\n%s", key, newUsage())
			}
			seenRootFSSize = true
			parsed, err := parseSize(value, mib)
			if err != nil {
				return "", provision.CreateOptions{}, nil, fmt.Errorf("parse %s: %v\n%s", key, err, newUsage())
			}
			opts.RootFSSizeBytes = parsed
		case "--integration":
			integrationName := strings.TrimSpace(value)
			if !validIntegrationName.MatchString(integrationName) {
				return "", provision.CreateOptions{}, nil, fmt.Errorf("invalid integration name %q\n%s", integrationName, newUsage())
			}
			integrations = append(integrations, integrationName)
		default:
			return "", provision.CreateOptions{}, nil, fmt.Errorf("unknown option %q\n%s", key, newUsage())
		}
	}

	if name == "" {
		return "", provision.CreateOptions{}, nil, errors.New(newUsage())
	}
	if opts.MemoryPoolName != "" && !seenRAM {
		return "", provision.CreateOptions{}, nil, fmt.Errorf("new --pool requires --ram\n%s", newUsage())
	}
	return name, opts, integrations, nil
}

func parseIntegrationAddArgs(args []string) (string, string, integrationAddOptions, error) {
	if len(args) < 5 {
		return "", "", integrationAddOptions{}, errors.New(integrationAddUsage())
	}
	kind := strings.TrimSpace(args[2])
	name := strings.TrimSpace(args[3])
	if kind == "" || !validIntegrationName.MatchString(name) {
		return "", "", integrationAddOptions{}, fmt.Errorf("invalid integration name %q\n%s", name, integrationAddUsage())
	}

	var (
		opts                 integrationAddOptions
		seenTarget           bool
		seenBearerEnv        bool
		seenBasicUser        bool
		seenBasicPasswordEnv bool
	)
	opts.AuthMode = model.IntegrationAuthNone

	for i := 4; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "--") {
			return "", "", integrationAddOptions{}, fmt.Errorf("unexpected argument %q\n%s", arg, integrationAddUsage())
		}
		key, value, hasValue := strings.Cut(arg, "=")
		if !hasValue {
			i++
			if i >= len(args) {
				return "", "", integrationAddOptions{}, fmt.Errorf("missing value for %s\n%s", key, integrationAddUsage())
			}
			value = args[i]
		}
		switch key {
		case "--target":
			if seenTarget {
				return "", "", integrationAddOptions{}, fmt.Errorf("%s specified more than once\n%s", key, integrationAddUsage())
			}
			seenTarget = true
			parsed, err := parseIntegrationTarget(value)
			if err != nil {
				return "", "", integrationAddOptions{}, fmt.Errorf("parse %s: %v\n%s", key, err, integrationAddUsage())
			}
			opts.TargetURL = parsed
		case "--header":
			header, err := parseStaticIntegrationHeader(value)
			if err != nil {
				return "", "", integrationAddOptions{}, fmt.Errorf("parse %s: %v\n%s", key, err, integrationAddUsage())
			}
			opts.Headers = append(opts.Headers, header)
		case "--header-env":
			header, err := parseEnvIntegrationHeader(value)
			if err != nil {
				return "", "", integrationAddOptions{}, fmt.Errorf("parse %s: %v\n%s", key, err, integrationAddUsage())
			}
			opts.Headers = append(opts.Headers, header)
		case "--bearer-env":
			if seenBearerEnv {
				return "", "", integrationAddOptions{}, fmt.Errorf("%s specified more than once\n%s", key, integrationAddUsage())
			}
			seenBearerEnv = true
			if err := validateIntegrationSecretEnv(value); err != nil {
				return "", "", integrationAddOptions{}, fmt.Errorf("parse %s: %v\n%s", key, err, integrationAddUsage())
			}
			opts.BearerEnv = strings.TrimSpace(value)
		case "--basic-user":
			if seenBasicUser {
				return "", "", integrationAddOptions{}, fmt.Errorf("%s specified more than once\n%s", key, integrationAddUsage())
			}
			seenBasicUser = true
			opts.BasicUser = strings.TrimSpace(value)
			if opts.BasicUser == "" {
				return "", "", integrationAddOptions{}, fmt.Errorf("parse %s: value cannot be empty\n%s", key, integrationAddUsage())
			}
		case "--basic-password-env":
			if seenBasicPasswordEnv {
				return "", "", integrationAddOptions{}, fmt.Errorf("%s specified more than once\n%s", key, integrationAddUsage())
			}
			seenBasicPasswordEnv = true
			if err := validateIntegrationSecretEnv(value); err != nil {
				return "", "", integrationAddOptions{}, fmt.Errorf("parse %s: %v\n%s", key, err, integrationAddUsage())
			}
			opts.BasicPasswordEnv = strings.TrimSpace(value)
		default:
			return "", "", integrationAddOptions{}, fmt.Errorf("unknown option %q\n%s", key, integrationAddUsage())
		}
	}

	if !seenTarget {
		return "", "", integrationAddOptions{}, errors.New(integrationAddUsage())
	}
	if opts.BearerEnv != "" && (opts.BasicUser != "" || opts.BasicPasswordEnv != "") {
		return "", "", integrationAddOptions{}, fmt.Errorf("bearer auth and basic auth cannot be combined\n%s", integrationAddUsage())
	}
	if opts.BearerEnv != "" {
		opts.AuthMode = model.IntegrationAuthBearerEnv
	}
	if opts.BasicUser != "" || opts.BasicPasswordEnv != "" {
		if opts.BasicUser == "" || opts.BasicPasswordEnv == "" {
			return "", "", integrationAddOptions{}, fmt.Errorf("basic auth requires both --basic-user and --basic-password-env\n%s", integrationAddUsage())
		}
		opts.AuthMode = model.IntegrationAuthBasicEnv
	}
	return kind, name, opts, nil
}

func parseIntegrationTarget(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("target scheme must be http or https")
	}
	if parsed.Host == "" {
		return "", errors.New("target host is required")
	}
	if parsed.User != nil {
		return "", errors.New("target URL must not include user info")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("target URL must not include a query string or fragment")
	}
	return parsed.String(), nil
}

func parseStaticIntegrationHeader(raw string) (model.IntegrationHeader, error) {
	name, value, ok := strings.Cut(raw, ":")
	if !ok {
		return model.IntegrationHeader{}, errors.New("expected NAME:VALUE")
	}
	name = strings.TrimSpace(name)
	value = strings.TrimSpace(value)
	if name == "" || value == "" {
		return model.IntegrationHeader{}, errors.New("header name and value must be non-empty")
	}
	if !validHTTPHeaderName(name) {
		return model.IntegrationHeader{}, fmt.Errorf("invalid header name %q", name)
	}
	return model.IntegrationHeader{Name: name, Value: value}, nil
}

func parseEnvIntegrationHeader(raw string) (model.IntegrationHeader, error) {
	name, envName, ok := strings.Cut(raw, ":")
	if !ok {
		return model.IntegrationHeader{}, errors.New("expected NAME:ENV")
	}
	name = strings.TrimSpace(name)
	envName = strings.TrimSpace(envName)
	if name == "" || envName == "" {
		return model.IntegrationHeader{}, errors.New("header name and env must be non-empty")
	}
	if !validHTTPHeaderName(name) {
		return model.IntegrationHeader{}, fmt.Errorf("invalid header name %q", name)
	}
	if err := validateIntegrationSecretEnv(envName); err != nil {
		return model.IntegrationHeader{}, err
	}
	return model.IntegrationHeader{Name: name, Env: envName}, nil
}

func validHTTPHeaderName(name string) bool {
	return validIntegrationHeaderName.MatchString(strings.TrimSpace(name))
}

func validateIntegrationSecretEnv(raw string) error {
	name := strings.TrimSpace(raw)
	if name == "" {
		return errors.New("secret env name cannot be empty")
	}
	if !strings.HasPrefix(name, integrationSecretEnvPrefix) {
		return fmt.Errorf("secret env name must start with %s", integrationSecretEnvPrefix)
	}
	return nil
}

func parseIntegrationNameAction(args []string, usage string) (string, error) {
	if len(args) != 3 || strings.TrimSpace(args[2]) == "" {
		return "", errors.New(usage)
	}
	return strings.TrimSpace(args[2]), nil
}

func parseIntegrationBindingArgs(args []string, usage string) (string, string, error) {
	if len(args) != 4 {
		return "", "", errors.New(usage)
	}
	vmName := strings.TrimSpace(args[2])
	integrationName := strings.TrimSpace(args[3])
	if vmName == "" || integrationName == "" {
		return "", "", errors.New(usage)
	}
	return vmName, integrationName, nil
}

func parseIntegrationListEnabledArgs(args []string) (string, error) {
	if len(args) != 3 || strings.TrimSpace(args[2]) == "" {
		return "", errors.New(integrationListEnabledUsage())
	}
	return strings.TrimSpace(args[2]), nil
}

func integrationUsage() string {
	return strings.Join([]string{
		"usage: integration <list|inspect|add|delete|enable|disable|list-enabled> ...",
		integrationListUsage(),
		integrationInspectUsage(),
		integrationAddUsage(),
		integrationDeleteUsage(),
		integrationEnableUsage(),
		integrationDisableUsage(),
		integrationListEnabledUsage(),
	}, "\n")
}

func integrationListUsage() string {
	return "usage: integration list"
}

func integrationInspectUsage() string {
	return "usage: integration inspect <name>"
}

func integrationAddUsage() string {
	return "usage: integration add http <name> --target URL [--header NAME:VALUE] [--header-env NAME:SRV_SECRET_FOO] [--bearer-env SRV_SECRET_FOO] [--basic-user USER --basic-password-env SRV_SECRET_BAR]"
}

func integrationDeleteUsage() string {
	return "usage: integration delete <name>"
}

func integrationEnableUsage() string {
	return "usage: integration enable <vm> <name>"
}

func integrationDisableUsage() string {
	return "usage: integration disable <vm> <name>"
}

func integrationListEnabledUsage() string {
	return "usage: integration list-enabled <vm>"
}
