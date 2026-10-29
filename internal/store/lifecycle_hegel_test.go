package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"hegel.dev/go/hegel"

	"srv/internal/model"
)

var lifecycleCreatedAt = time.Date(2026, 4, 5, 12, 0, 0, 0, time.UTC)
var lifecycleNames = []string{"alpha", "beta", "gamma"}

type lifecycleModelInstance struct {
	fixture     model.Instance
	state       string
	fence       string
	receipt     *model.CleanupReceipt
	importPhase string
}

type lifecycleModelRequest struct {
	id        string
	actor     string
	command   string
	name      string
	arguments string
	phase     string
	exitCode  int
}

type lifecycleModelImport struct {
	name            string
	receipt         model.ImportReceipt
	instanceCreated bool
}

type lifecycleMachine struct {
	store       *Store
	instances   map[string]*lifecycleModelInstance
	requests    map[string]*lifecycleModelRequest
	imports     map[string]*lifecycleModelImport
	nextRequest int
	nextImport  int
}

func newLifecycleMachine(st *Store) *lifecycleMachine {
	return &lifecycleMachine{
		store:     st,
		instances: make(map[string]*lifecycleModelInstance),
		requests:  make(map[string]*lifecycleModelRequest),
		imports:   make(map[string]*lifecycleModelImport),
	}
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func drawKey[V any](tc hegel.TestCase, values map[string]V) (string, bool) {
	keys := sortedKeys(values)
	if len(keys) == 0 {
		return "", false
	}
	return keys[hegel.Draw(tc, hegel.Integers(0, len(keys)-1))], true
}

func (m *lifecycleMachine) RuleCreate(tc hegel.TestCase) {
	name := lifecycleNames[hegel.Draw(tc, hegel.Integers(0, len(lifecycleNames)-1))]
	if _, exists := m.instances[name]; exists {
		return
	}
	tc.Event("create")
	inst := testInstance(name, model.StateStopped, lifecycleCreatedAt)
	inst.Kind, inst.Image, inst.TailnetNodeID = model.KindOrb, "amp-orb", ""
	if err := m.store.CreateInstance(context.Background(), inst); err != nil {
		tc.Errorf("create %s: %v", name, err)
		return
	}
	m.instances[name] = &lifecycleModelInstance{fixture: inst, state: model.StateStopped}
}

func (m *lifecycleMachine) RuleAdmit(tc hegel.TestCase) {
	name, ok := drawKey(tc, m.instances)
	if !ok {
		return
	}
	command := []string{"start", "delete-proof"}[hegel.Draw(tc, hegel.Integers(0, 1))]
	m.nextRequest++
	id := fmt.Sprintf("req-%d", m.nextRequest)
	request := LifecycleRequest{ID: id, Actor: "owner", Command: command, Name: name, Arguments: "bound"}
	admitted, err := m.store.AdmitLifecycleRequest(context.Background(), request)
	if err != nil || !admitted {
		tc.Errorf("admit fresh %s: admitted=%v err=%v", id, admitted, err)
		return
	}
	tc.Event("admit-fresh/" + command)
	m.requests[id] = &lifecycleModelRequest{id: id, actor: "owner", command: command, name: name, arguments: "bound", phase: "admitted"}
}

func (m *lifecycleMachine) RuleAdmitExisting(tc hegel.TestCase) {
	id, ok := drawKey(tc, m.requests)
	if !ok {
		return
	}
	expected := m.requests[id]
	changed := hegel.Draw(tc, hegel.Booleans())
	arguments := expected.arguments
	if changed {
		arguments = "changed"
	}
	admitted, err := m.store.AdmitLifecycleRequest(context.Background(), LifecycleRequest{
		ID:        id,
		Actor:     expected.actor,
		Command:   expected.command,
		Name:      expected.name,
		Arguments: arguments,
	})
	if changed {
		tc.Event("admit-changed")
		if err == nil || admitted {
			tc.Errorf("rebinding request %s succeeded: admitted=%v err=%v", id, admitted, err)
		}
		return
	}
	tc.Event("admit-same")
	if err != nil || admitted {
		tc.Errorf("re-admitting request %s: admitted=%v err=%v", id, admitted, err)
	}
}

func (m *lifecycleMachine) RuleBeginRequest(tc hegel.TestCase) {
	id, ok := drawKey(tc, m.requests)
	if !ok {
		return
	}
	expected := m.requests[id]
	if expected.command != "start" {
		return
	}
	err := m.store.BeginLifecycleRequest(context.Background(), id, expected.actor)
	if expected.phase == "admitted" {
		tc.Event("begin-request-succeeded")
		if err != nil {
			tc.Errorf("begin request %s: %v", id, err)
			return
		}
		expected.phase = "executing"
		return
	}
	tc.Event("begin-request-rejected")
	if err == nil {
		tc.Errorf("begin request %s succeeded in phase %q", id, expected.phase)
	}
}

func (m *lifecycleMachine) RuleFinishRequest(tc hegel.TestCase) {
	id, ok := drawKey(tc, m.requests)
	if !ok {
		return
	}
	expected := m.requests[id]
	if expected.command != "start" {
		return
	}
	exitCode := []int{0, 1}[hegel.Draw(tc, hegel.Integers(0, 1))]
	finished, err := m.store.FinishLifecycleRequest(context.Background(), id, expected.actor, exitCode)
	if err != nil {
		tc.Errorf("finish request %s: %v", id, err)
		return
	}
	switch {
	case expected.phase == "executing":
		tc.Event("finish-succeeded")
		if !finished {
			tc.Errorf("finish executing request %s did not finish", id)
			return
		}
		expected.exitCode = exitCode
		if exitCode == 0 {
			expected.phase = "done"
		} else {
			expected.phase = "failed"
		}
	case expected.phase == "admitted" && exitCode != 0:
		tc.Event("finish-succeeded")
		if !finished {
			tc.Errorf("finish admitted request %s with exit %d did not finish", id, exitCode)
			return
		}
		expected.exitCode = exitCode
		expected.phase = "failed"
	default:
		tc.Event("finish-rejected")
		if finished {
			tc.Errorf("finish request %s succeeded in phase %q with exit %d", id, expected.phase, exitCode)
		}
	}
}

func (m *lifecycleMachine) RuleBeginCleanup(tc hegel.TestCase) {
	name, ok := drawKey(tc, m.instances)
	if !ok {
		return
	}
	expected := m.instances[name]
	requestIDs := make([]string, 0)
	for id, request := range m.requests {
		if request.command == "delete-proof" && request.phase == "admitted" {
			requestIDs = append(requestIDs, id)
		}
	}
	sort.Strings(requestIDs)
	requestID := "missing"
	if len(requestIDs) != 0 {
		requestID = requestIDs[hegel.Draw(tc, hegel.Integers(0, len(requestIDs)-1))]
	}
	matching := requestID != "missing" && m.requests[requestID].name == name
	valid := matching &&
		(expected.state == model.StateReady || expected.state == model.StateStopped || expected.state == model.StateProvisioning) &&
		expected.fence == ""

	stored, err := m.store.GetInstance(context.Background(), name)
	if err != nil {
		tc.Errorf("get %s: %v", name, err)
		return
	}
	err = m.store.BeginCleanup(context.Background(), requestID, "owner", stored)
	if valid {
		tc.Event("begin-succeeded")
		if err != nil {
			tc.Errorf("begin cleanup %s: %v", name, err)
			return
		}
		m.requests[requestID].phase = "executing"
		expected.state = model.StateDeleting
		expected.fence = requestID
		return
	}
	tc.Event("begin-rejected")
	if err == nil {
		tc.Errorf("begin cleanup for %s succeeded in state %q kind %q fence %q request %q", name, expected.state, expected.fixture.KindName(), expected.fence, requestID)
	}
}

func (m *lifecycleMachine) RuleCommitCleanup(tc hegel.TestCase) {
	name, ok := drawKey(tc, m.instances)
	if !ok {
		return
	}
	expected := m.instances[name]
	requestID := expected.fence
	valid := requestID != "" && m.requests[requestID].phase == "executing" &&
		expected.state == model.StateDeleting
	receipt := cleanupReceiptFor(expected.fixture, requestID)
	err := m.store.CommitCleanup(context.Background(), receipt)
	if valid {
		tc.Event("commit-succeeded")
		if err != nil {
			tc.Errorf("commit cleanup %s: %v", name, err)
			return
		}
		m.requests[requestID].phase = "done"
		m.requests[requestID].exitCode = 0
		expected.state = model.StateDeleted
		expected.fence = ""
		expected.receipt = &receipt
		return
	}
	tc.Event("commit-rejected")
	if err == nil {
		tc.Errorf("commit cleanup for %s succeeded without a valid fence (request %q)", name, requestID)
	}
}

func (m *lifecycleMachine) RuleReserveImport(tc hegel.TestCase) {
	m.nextImport++
	name := fmt.Sprintf("import-%d", m.nextImport)
	inst := testInstance(name, model.StateProvisioning, lifecycleCreatedAt)
	receipt := model.ImportReceipt{
		InstanceID:       inst.ID,
		Token:            "import-token",
		SourceInstanceID: "source-id",
		ExportedAt:       "2026-04-05T12:00:00Z",
		Phase:            "importing",
	}
	if err := m.store.ReserveImportReceipt(context.Background(), receipt); err != nil {
		tc.Errorf("reserve import %s: %v", name, err)
		return
	}
	tc.Event("reserve-fresh")
	m.imports[inst.ID] = &lifecycleModelImport{name: name, receipt: receipt}
}

func (m *lifecycleMachine) RuleReserveImportExisting(tc hegel.TestCase) {
	id, ok := drawKey(tc, m.imports)
	if !ok {
		return
	}
	expected := m.imports[id]
	receipt := model.ImportReceipt{
		InstanceID:       id,
		Token:            fmt.Sprintf("retry-token-%d", m.nextImport),
		SourceInstanceID: "retry-source",
		ExportedAt:       "2026-04-06T12:00:00Z",
		Phase:            "importing",
	}
	_, instanceExists := m.instances[expected.name]
	err := m.store.ReserveImportReceipt(context.Background(), receipt)
	if instanceExists {
		tc.Event("reserve-existing-rejected")
		if err == nil {
			tc.Errorf("reserve import %s succeeded with an existing instance", expected.name)
		}
		return
	}
	tc.Event("reserve-existing")
	if err != nil {
		tc.Errorf("reserve existing import %s: %v", expected.name, err)
		return
	}
	expected.receipt = receipt
}

func (m *lifecycleMachine) RuleReserveImportCollision(tc hegel.TestCase) {
	ids := make([]string, 0)
	for id, imp := range m.imports {
		if _, exists := m.instances[imp.name]; exists {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return
	}
	sort.Strings(ids)
	id := ids[hegel.Draw(tc, hegel.Integers(0, len(ids)-1))]
	imp := m.imports[id]
	receipt := model.ImportReceipt{
		InstanceID:       id,
		Token:            "collision-token",
		SourceInstanceID: "collision-source",
		ExportedAt:       "2026-04-07T12:00:00Z",
		Phase:            "importing",
	}
	tc.Event("reserve-collision")
	if err := m.store.ReserveImportReceipt(context.Background(), receipt); err == nil {
		tc.Errorf("reserve import %s succeeded with an existing instance", imp.name)
	}
}

func (m *lifecycleMachine) RuleCreateImportInstance(tc hegel.TestCase) {
	ids := make([]string, 0)
	for id, imp := range m.imports {
		if !imp.instanceCreated {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return
	}
	sort.Strings(ids)
	id := ids[hegel.Draw(tc, hegel.Integers(0, len(ids)-1))]
	imp := m.imports[id]
	inst := testInstance(imp.name, model.StateProvisioning, lifecycleCreatedAt)
	inst.ID = id
	if err := m.store.CreateInstance(context.Background(), inst); err != nil {
		tc.Errorf("create importing instance %s: %v", imp.name, err)
		return
	}
	tc.Event("create-import-instance")
	imp.instanceCreated = true
	m.instances[imp.name] = &lifecycleModelInstance{fixture: inst, state: model.StateProvisioning, importPhase: "importing"}
}

func (m *lifecycleMachine) RuleFinalizeImport(tc hegel.TestCase) {
	id, ok := drawKey(tc, m.imports)
	if !ok {
		return
	}
	imp := m.imports[id]
	instance, instanceExists := m.instances[imp.name]
	valid := imp.receipt.Phase == "importing" && instanceExists && instance.state == model.StateProvisioning
	err := m.store.FinalizeImport(context.Background(), id, time.Now().UTC())
	if valid {
		tc.Event("finalize-succeeded")
		if err != nil {
			tc.Errorf("finalize import %s: %v", imp.name, err)
			return
		}
		instance.state = model.StateStopped
		instance.importPhase = "imported"
		imp.receipt.Phase = "imported"
		return
	}
	tc.Event("finalize-rejected")
	if err == nil {
		tc.Errorf("finalize import %s succeeded without an importing receipt and instance", imp.name)
	}
}

func (m *lifecycleMachine) RuleInterrupt(tc hegel.TestCase) {
	if err := m.store.InterruptLifecycleRequests(context.Background()); err != nil {
		tc.Errorf("interrupt requests: %v", err)
		return
	}
	for _, request := range m.requests {
		switch request.phase {
		case "admitted":
			request.phase = "cancelled"
		case "executing":
			request.phase = "interrupted"
		}
	}
}

func (m *lifecycleMachine) InvariantInstances(tc hegel.TestCase) {
	ctx := context.Background()
	for _, name := range sortedKeys(m.instances) {
		expected := m.instances[name]
		inst, err := m.store.GetInstance(ctx, name)
		if err != nil {
			tc.Errorf("instance %s: %v", name, err)
			return
		}
		if inst.ID != expected.fixture.ID || inst.State != expected.state ||
			inst.KindName() != expected.fixture.KindName() || inst.ImageName() != expected.fixture.ImageName() ||
			inst.RootFSPath != expected.fixture.RootFSPath || inst.TapDevice != expected.fixture.TapDevice ||
			inst.NetworkCIDR != expected.fixture.NetworkCIDR {
			tc.Errorf("instance %s = %+v, want fixture %+v state %q", name, inst, expected.fixture, expected.state)
			return
		}
		if expected.state == model.StateDeleted && inst.DeletedAt == nil {
			tc.Errorf("instance %s is deleted without a timestamp", name)
			return
		}
	}
	all, err := m.store.ListInstances(ctx, true)
	if err != nil {
		tc.Errorf("list instances: %v", err)
		return
	}
	if len(all) != len(m.instances) {
		tc.Errorf("store has %d instances, model has %d", len(all), len(m.instances))
	}
}

func (m *lifecycleMachine) InvariantRequests(tc hegel.TestCase) {
	ctx := context.Background()
	for _, id := range sortedKeys(m.requests) {
		expected := m.requests[id]
		request, err := m.store.GetLifecycleRequest(ctx, id, expected.actor)
		if err != nil {
			tc.Errorf("request %s: %v", id, err)
			return
		}
		if request.Phase != expected.phase || request.ExitCode != expected.exitCode ||
			request.Actor != expected.actor || request.Command != expected.command ||
			request.Name != expected.name || request.Arguments != expected.arguments {
			tc.Errorf("request %s = %+v, want %+v", id, request, expected)
			return
		}
		switch request.Phase {
		case "done":
			if request.ExitCode != 0 {
				tc.Errorf("request %s is done with exit %d", id, request.ExitCode)
			}
		case "failed":
			if request.ExitCode == 0 {
				tc.Errorf("request %s failed with exit 0", id)
			}
		case "admitted", "executing", "cancelled", "interrupted":
		default:
			tc.Errorf("request %s has unknown phase %q", id, request.Phase)
		}
	}
}

func (m *lifecycleMachine) InvariantFences(tc hegel.TestCase) {
	ctx := context.Background()
	for _, name := range sortedKeys(m.instances) {
		expected := m.instances[name]
		fenced := m.store.CheckCleanupFence(ctx, expected.fixture.ID) != nil
		if fenced != (expected.fence != "") {
			tc.Errorf("instance %s fence = %v, model %q", name, fenced, expected.fence)
			return
		}
		if expected.fence != "" {
			request := m.requests[expected.fence]
			if expected.state != model.StateDeleting || request == nil ||
				(request.phase != "executing" && request.phase != "interrupted") {
				tc.Errorf("fenced instance %s state %q request %+v", name, expected.state, request)
				return
			}
			if _, err := m.store.CleanupReceipt(ctx, expected.fence, "owner"); !errors.Is(err, sql.ErrNoRows) {
				tc.Errorf("fenced instance %s already has a receipt: %v", name, err)
				return
			}
		}
		if expected.receipt != nil {
			stored, err := m.store.CleanupReceipt(ctx, expected.receipt.RequestID, expected.receipt.Actor)
			if err != nil || !reflect.DeepEqual(stored, *expected.receipt) {
				tc.Errorf("receipt for %s = %+v, %v; want %+v", name, stored, err, *expected.receipt)
				return
			}
			if expected.state != model.StateDeleted {
				tc.Errorf("receipt instance %s state = %q", name, expected.state)
				return
			}
			request := m.requests[expected.receipt.RequestID]
			if request == nil || request.phase != "done" || request.exitCode != 0 {
				tc.Errorf("receipt request %q = %+v", expected.receipt.RequestID, request)
				return
			}
		}
	}
}

func (m *lifecycleMachine) InvariantImports(tc hegel.TestCase) {
	ctx := context.Background()
	for _, id := range sortedKeys(m.imports) {
		expected := m.imports[id]
		stored, err := m.store.GetImportReceipt(ctx, id)
		if err != nil {
			tc.Errorf("import receipt %s: %v", id, err)
			return
		}
		if stored != expected.receipt {
			tc.Errorf("import receipt %s = %+v, want %+v", id, stored, expected.receipt)
			return
		}
		instance, exists := m.instances[expected.name]
		if !exists {
			continue
		}
		if instance.importPhase != stored.Phase {
			tc.Errorf("import instance %s phase = %q, receipt %q", expected.name, instance.importPhase, stored.Phase)
			return
		}
		// An imported receipt records completed import, not a perpetual stopped
		// state: later lifecycle operations may move the instance.
		if stored.Phase == "importing" && instance.state != model.StateProvisioning && instance.state != model.StateDeleting && instance.state != model.StateDeleted {
			tc.Errorf("importing instance %s state = %q", expected.name, instance.state)
			return
		}
	}
}

func (m *lifecycleMachine) InvariantTerminalRequests(tc hegel.TestCase) {
	ctx := context.Background()
	for _, id := range sortedKeys(m.requests) {
		expected := m.requests[id]
		admitted, err := m.store.AdmitLifecycleRequest(ctx, LifecycleRequest{
			ID: expected.id, Actor: expected.actor, Command: expected.command, Name: expected.name, Arguments: expected.arguments + "-changed",
		})
		if err == nil {
			tc.Errorf("request %s accepted a changed binding: admitted=%v", id, admitted)
			return
		}
		switch expected.phase {
		case "admitted":
			finished, err := m.store.FinishLifecycleRequest(ctx, id, expected.actor, 0)
			if err != nil || finished {
				tc.Errorf("admitted request %s finished with exit 0: finished=%v err=%v", id, finished, err)
				return
			}
			continue
		case "done", "failed", "cancelled", "interrupted":
		default:
			continue
		}
		admitted, err = m.store.AdmitLifecycleRequest(ctx, LifecycleRequest{
			ID: expected.id, Actor: expected.actor, Command: expected.command, Name: expected.name, Arguments: expected.arguments,
		})
		if err != nil || admitted {
			tc.Errorf("terminal request %s re-admitted: admitted=%v err=%v", id, admitted, err)
			return
		}
		if err := m.store.BeginLifecycleRequest(ctx, id, expected.actor); err == nil {
			tc.Errorf("terminal request %s began execution", id)
			return
		}
		finished, err := m.store.FinishLifecycleRequest(ctx, id, expected.actor, 0)
		if err != nil || finished {
			tc.Errorf("terminal request %s finished again: finished=%v err=%v", id, finished, err)
			return
		}
	}
}

func cleanupReceiptFor(inst model.Instance, requestID string) model.CleanupReceipt {
	authDisposition := "not-asserted"
	if inst.KindName() == model.KindOrb {
		authDisposition = "never-minted"
	}
	return model.CleanupReceipt{
		Version:            5,
		BackendID:          "backend-one",
		Kind:               inst.KindName(),
		Image:              inst.ImageName(),
		RequestID:          requestID,
		Actor:              "owner",
		InstanceID:         inst.ID,
		Name:               inst.Name,
		CreatedAt:          inst.CreatedAt,
		CreatedByUser:      inst.CreatedByUser,
		CreatedByNode:      inst.CreatedByNode,
		AuthKeyDisposition: authDisposition,
		TailnetDisposition: "absent",
		IngressClosed:      true,
		GatewaysAbsent:     true,
		RuntimeAbsent:      true,
		JailerAbsent:       true,
		CgroupAbsent:       true,
		NetworkAbsent:      true,
		TapDevice:          inst.TapDevice,
		NetworkCIDR:        inst.NetworkCIDR,
		InstanceDir:        filepath.Dir(inst.RootFSPath),
		RootFSPath:         inst.RootFSPath,
		BackupsDir:         filepath.Join("/backups", inst.Name),
		StorageAbsent:      true,
		DeletedAt:          inst.CreatedAt.Add(time.Hour),
	}
}

// seed starts every case with one cleanup candidate and one already-fenced
// instance. Without it the generator spends most steps discovering the
// create -> admit -> begin prefix.
func (m *lifecycleMachine) seed(tc hegel.TestCase) {
	for _, name := range []string{"alpha", "beta"} {
		inst := testInstance(name, model.StateStopped, lifecycleCreatedAt)
		inst.Kind, inst.Image, inst.TailnetNodeID = model.KindOrb, "amp-orb", ""
		if err := m.store.CreateInstance(context.Background(), inst); err != nil {
			tc.Errorf("seed create %s: %v", name, err)
			return
		}
		m.instances[name] = &lifecycleModelInstance{fixture: inst, state: model.StateStopped}
	}
	m.seedAdmitted(tc, "alpha", "req-seed")
	m.seedAdmitted(tc, "beta", "req-seed-beta")

	importName := "import-seed"
	importInst := testInstance(importName, model.StateProvisioning, lifecycleCreatedAt)
	importReceipt := model.ImportReceipt{
		InstanceID:       importInst.ID,
		Token:            "seed-token",
		SourceInstanceID: "seed-source",
		ExportedAt:       "2026-04-05T12:00:00Z",
		Phase:            "importing",
	}
	if err := m.store.ReserveImportReceipt(context.Background(), importReceipt); err != nil {
		tc.Errorf("seed reserve import: %v", err)
		return
	}
	if err := m.store.CreateInstance(context.Background(), importInst); err != nil {
		tc.Errorf("seed create import instance: %v", err)
		return
	}
	m.imports[importInst.ID] = &lifecycleModelImport{name: importName, receipt: importReceipt, instanceCreated: true}
	m.instances[importName] = &lifecycleModelInstance{fixture: importInst, state: model.StateProvisioning, importPhase: "importing"}

	ctx := context.Background()
	beta, err := m.store.GetInstance(ctx, "beta")
	if err != nil {
		tc.Errorf("seed get beta: %v", err)
		return
	}
	if err := m.store.BeginCleanup(ctx, "req-seed-beta", "owner", beta); err != nil {
		tc.Errorf("seed begin beta: %v", err)
		return
	}
	m.requests["req-seed-beta"].phase = "executing"
	m.instances["beta"].state = model.StateDeleting
	m.instances["beta"].fence = "req-seed-beta"
}

func (m *lifecycleMachine) seedAdmitted(tc hegel.TestCase, name, id string) {
	request := LifecycleRequest{ID: id, Actor: "owner", Command: "delete-proof", Name: name, Arguments: "bound"}
	admitted, err := m.store.AdmitLifecycleRequest(context.Background(), request)
	if err != nil || !admitted {
		tc.Errorf("seed admit %s: admitted=%v err=%v", id, admitted, err)
		return
	}
	m.requests[id] = &lifecycleModelRequest{id: id, actor: request.Actor, command: request.Command, name: request.Name, arguments: request.Arguments, phase: "admitted"}
}

func TestLifecycleStateMachine(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		st, err := Open(filepath.Join(ht.TempDir(), "state.db"))
		if err != nil {
			ht.Fatal(err)
		}
		defer st.Close()
		machine := newLifecycleMachine(st)
		machine.seed(ht)
		hegel.RunStateful(
			ht,
			machine,
			hegel.WithStatefulStepCount(15),
			hegel.WithAlwaysCheckInvariants(
				"InvariantInstances",
				"InvariantRequests",
				"InvariantFences",
				"InvariantImports",
				"InvariantTerminalRequests",
			),
		)
	}, hegel.WithTestCases(40))
}
