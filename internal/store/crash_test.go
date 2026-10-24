package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"srv/internal/model"
)

// Crash consistency is checked at operation boundaries, not inside SQLite
// transactions. A SIGKILL after any committed step must leave only the exact
// durable prefix the completed operations produced: unfinished requests are
// cancelled or interrupted, a fence exists only while a delete is unresolved,
// and a receipt exists only with a deleted instance and a done request.
const (
	crashScriptEnv = "SRV_STORE_CRASH_SCRIPT"
	crashAtEnv     = "SRV_STORE_CRASH_AT"
	crashDBEnv     = "SRV_STORE_CRASH_DB"
)

type crashStep func(context.Context) error

type crashExpectation struct {
	instanceState string
	requestPhase  string
	fenced        bool
	receipt       bool
	importPhase   string
}

type crashPlan struct {
	steps         []crashStep
	instanceName  string
	instanceID    string
	requestID     string
	actor         string
	command       string
	arguments     string
	admittedAfter int
	expected      []crashExpectation
}

func crashPlanFor(script string, s *Store) (*crashPlan, error) {
	switch script {
	case "cleanup":
		inst, receipt := cleanupValues()
		request := LifecycleRequest{ID: receipt.RequestID, Actor: receipt.Actor, Command: "delete-proof", Name: inst.Name, Arguments: "bound-arguments"}
		return &crashPlan{
			steps: []crashStep{
				func(ctx context.Context) error { return s.CreateInstance(ctx, inst) },
				func(ctx context.Context) error {
					ok, err := s.AdmitLifecycleRequest(ctx, request)
					if err != nil {
						return err
					}
					if !ok {
						return errors.New("cleanup request was not admitted")
					}
					return nil
				},
				func(ctx context.Context) error { return s.BeginCleanup(ctx, request.ID, request.Actor, inst) },
				func(ctx context.Context) error { return s.CommitCleanup(ctx, receipt) },
			},
			instanceName:  inst.Name,
			instanceID:    inst.ID,
			requestID:     request.ID,
			actor:         request.Actor,
			command:       request.Command,
			arguments:     request.Arguments,
			admittedAfter: 2,
			expected: []crashExpectation{
				{},
				{instanceState: model.StateStopped},
				{instanceState: model.StateStopped, requestPhase: "cancelled"},
				{instanceState: model.StateDeleting, requestPhase: "interrupted", fenced: true},
				{instanceState: model.StateDeleted, requestPhase: "done", receipt: true},
			},
		}, nil
	case "import":
		inst := testInstance("crash-import", model.StateProvisioning, time.Date(2026, 10, 22, 1, 2, 3, 4, time.UTC))
		receipt := model.ImportReceipt{
			InstanceID:       inst.ID,
			Token:            "import-token",
			SourceInstanceID: "source-instance-id",
			ExportedAt:       "2026-10-22T01:02:03Z",
			Phase:            "importing",
		}
		return &crashPlan{
			steps: []crashStep{
				func(ctx context.Context) error { return s.ReserveImportReceipt(ctx, receipt) },
				func(ctx context.Context) error { return s.CreateInstance(ctx, inst) },
				func(ctx context.Context) error { return s.FinalizeImport(ctx, inst.ID, inst.UpdatedAt) },
			},
			instanceName: inst.Name,
			instanceID:   inst.ID,
			expected: []crashExpectation{
				{},
				{importPhase: "importing"},
				{instanceState: model.StateProvisioning, importPhase: "importing"},
				{instanceState: model.StateStopped, importPhase: "imported"},
			},
		}, nil
	case "lifecycle":
		inst := testInstance("crash-lifecycle", model.StateReady, time.Date(2026, 10, 22, 1, 2, 3, 4, time.UTC))
		request := LifecycleRequest{ID: "crash-lifecycle-request", Actor: "owner", Command: "start", Name: inst.Name, Arguments: "[]"}
		return &crashPlan{
			steps: []crashStep{
				func(ctx context.Context) error { return s.CreateInstance(ctx, inst) },
				func(ctx context.Context) error {
					ok, err := s.AdmitLifecycleRequest(ctx, request)
					if err != nil {
						return err
					}
					if !ok {
						return errors.New("lifecycle request was not admitted")
					}
					return nil
				},
				func(ctx context.Context) error { return s.BeginLifecycleRequest(ctx, request.ID, request.Actor) },
				func(ctx context.Context) error {
					ok, err := s.FinishLifecycleRequest(ctx, request.ID, request.Actor, 0)
					if err != nil {
						return err
					}
					if !ok {
						return errors.New("lifecycle request did not finish")
					}
					return nil
				},
			},
			instanceName:  inst.Name,
			instanceID:    inst.ID,
			requestID:     request.ID,
			actor:         request.Actor,
			command:       request.Command,
			arguments:     request.Arguments,
			admittedAfter: 2,
			expected: []crashExpectation{
				{},
				{instanceState: model.StateReady},
				{instanceState: model.StateReady, requestPhase: "cancelled"},
				{instanceState: model.StateReady, requestPhase: "interrupted"},
				{instanceState: model.StateReady, requestPhase: "done"},
			},
		}, nil
	default:
		return nil, fmt.Errorf("unknown crash script %q", script)
	}
}

// TestCrashChild is the re-executed side of TestCrashConsistency. It performs
// the scripted writes and kills itself at the requested boundary. A negative
// crash point runs every step and closes normally, which separates recovery
// checks from the clean-close path.
func TestCrashChild(t *testing.T) {
	script := os.Getenv(crashScriptEnv)
	if script == "" {
		t.Skip("crash consistency child process")
	}
	at, err := strconv.Atoi(os.Getenv(crashAtEnv))
	if err != nil {
		t.Fatalf("parse %s: %v", crashAtEnv, err)
	}
	s, err := Open(os.Getenv(crashDBEnv))
	if err != nil {
		t.Fatalf("open crash database: %v", err)
	}
	defer s.Close()
	plan, err := crashPlanFor(script, s)
	if err != nil {
		t.Fatal(err)
	}
	if at < -1 || at > len(plan.steps) {
		t.Fatalf("crash point %d outside -1..%d", at, len(plan.steps))
	}
	for i, step := range plan.steps {
		if i == at {
			killSelf(t)
		}
		if err := step(context.Background()); err != nil {
			t.Fatalf("%s step %d: %v", script, i, err)
		}
	}
	if at == len(plan.steps) {
		killSelf(t)
	}
}

func killSelf(t *testing.T) {
	t.Helper()
	if err := syscall.Kill(os.Getpid(), syscall.SIGKILL); err != nil {
		t.Fatalf("kill self: %v", err)
	}
	time.Sleep(time.Minute)
}

func TestCrashConsistency(t *testing.T) {
	for _, script := range []string{"cleanup", "import", "lifecycle"} {
		t.Run(script, func(t *testing.T) {
			probe, err := Open(filepath.Join(t.TempDir(), "probe.db"))
			if err != nil {
				t.Fatal(err)
			}
			plan, err := crashPlanFor(script, probe)
			if err := probe.Close(); err != nil {
				t.Fatal(err)
			}
			if err != nil {
				t.Fatal(err)
			}
			for at := 0; at <= len(plan.steps); at++ {
				t.Run(fmt.Sprintf("crash-at-%d", at), func(t *testing.T) {
					dbPath := filepath.Join(t.TempDir(), "state", "app.db")
					runCrashChild(t, script, at, dbPath)
					verifyCrashInvariants(t, dbPath, plan, at)
				})
			}
			t.Run("clean-close", func(t *testing.T) {
				dbPath := filepath.Join(t.TempDir(), "state", "app.db")
				runCrashChild(t, script, -1, dbPath)
				verifyCrashInvariants(t, dbPath, plan, len(plan.steps))
			})
		})
	}
}

func runCrashChild(t *testing.T, script string, at int, dbPath string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashChild$", "-test.count=1")
	cmd.Env = append(os.Environ(),
		crashScriptEnv+"="+script,
		crashAtEnv+"="+strconv.Itoa(at),
		crashDBEnv+"="+dbPath,
	)
	output, err := cmd.CombinedOutput()
	if at < 0 {
		if err != nil {
			t.Fatalf("clean child failed: %v\n%s", err, output)
		}
		return
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("child did not crash at step %d: %v\n%s", at, err, output)
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("child exited without SIGKILL at step %d: %v\n%s", at, exitErr, output)
	}
}

// verifyCrashInvariants checks the exact durable prefix for this cut, not only
// the rows that survived. A lost instance, request, receipt, or fence is a
// failure even when the remaining state is internally consistent.
func verifyCrashInvariants(t *testing.T, dbPath string, plan *crashPlan, completed int) {
	t.Helper()
	if completed < 0 || completed >= len(plan.expected) {
		t.Fatalf("completed steps %d outside 0..%d", completed, len(plan.expected)-1)
	}
	want := plan.expected[completed]
	ctx := context.Background()
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen crashed database: %v", err)
	}
	defer s.Close()
	if err := s.InterruptLifecycleRequests(ctx); err != nil {
		t.Fatalf("interrupt lifecycle requests: %v", err)
	}

	inst, found, err := s.FindInstance(ctx, plan.instanceName)
	if err != nil {
		t.Fatalf("find instance: %v", err)
	}
	if want.instanceState == "" {
		if found {
			t.Errorf("instance %s survived a cut before creation", plan.instanceName)
		}
	} else if !found {
		t.Errorf("instance %s was lost after %d completed steps", plan.instanceName, completed)
	} else {
		if inst.State != want.instanceState {
			t.Errorf("instance %s state = %q, want %q", plan.instanceName, inst.State, want.instanceState)
		}
		if inst.ID != plan.instanceID {
			t.Errorf("instance %s ID = %q, want %q", plan.instanceName, inst.ID, plan.instanceID)
		}
	}

	if plan.requestID != "" {
		request, err := s.GetLifecycleRequest(ctx, plan.requestID, plan.actor)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if want.requestPhase != "" {
				t.Errorf("request %s was lost after %d completed steps", plan.requestID, completed)
			}
		case err != nil:
			t.Fatalf("get lifecycle request: %v", err)
		default:
			if want.requestPhase == "" {
				t.Errorf("request %s survived a cut before admission", plan.requestID)
			} else if request.Phase != want.requestPhase {
				t.Errorf("request %s phase = %q, want %q", plan.requestID, request.Phase, want.requestPhase)
			}
			if request.Phase == "done" && request.ExitCode != 0 {
				t.Errorf("request %s is done with exit code %d", plan.requestID, request.ExitCode)
			}
		}
		if completed >= plan.admittedAfter {
			ok, err := s.AdmitLifecycleRequest(ctx, LifecycleRequest{
				ID:        plan.requestID,
				Actor:     plan.actor,
				Command:   plan.command,
				Name:      plan.instanceName,
				Arguments: plan.arguments,
			})
			if err != nil {
				t.Errorf("re-admit request %s: %v", plan.requestID, err)
			}
			if ok {
				t.Errorf("request %s was re-admitted after recovery", plan.requestID)
			}
		}
	}

	if fenced := s.CheckCleanupFence(ctx, plan.instanceID) != nil; fenced != want.fenced {
		t.Errorf("cleanup fence = %v, want %v", fenced, want.fenced)
	}

	if plan.requestID != "" {
		_, err := s.CleanupReceipt(ctx, plan.requestID, plan.actor)
		if receipt := err == nil; receipt != want.receipt {
			t.Errorf("cleanup receipt = %v, want %v", receipt, want.receipt)
		}
	}

	importReceipt, err := s.GetImportReceipt(ctx, plan.instanceID)
	if want.importPhase == "" {
		if err == nil {
			t.Errorf("unexpected import receipt %+v", importReceipt)
		}
	} else if err != nil {
		t.Errorf("import receipt for %s was lost: %v", plan.instanceID, err)
	} else if importReceipt.Phase != want.importPhase {
		t.Errorf("import receipt phase = %q, want %q", importReceipt.Phase, want.importPhase)
	}

	verifyCrashCrossRows(t, ctx, s)
}

// verifyCrashCrossRows checks relationships between whatever rows survived,
// including rows a scripted cut does not expect.
func verifyCrashCrossRows(t *testing.T, ctx context.Context, s *Store) {
	t.Helper()
	instances, err := s.ListInstances(ctx, true)
	if err != nil {
		t.Fatalf("list instances: %v", err)
	}
	byID := make(map[string]model.Instance, len(instances))
	for _, inst := range instances {
		byID[inst.ID] = inst
	}

	requests := make(map[string]LifecycleRequest)
	rows, err := s.db.QueryContext(ctx, `SELECT id, actor, command, name, arguments, phase, exit_code FROM lifecycle_requests`)
	if err != nil {
		t.Fatalf("query lifecycle requests: %v", err)
	}
	for rows.Next() {
		var r LifecycleRequest
		if err := rows.Scan(&r.ID, &r.Actor, &r.Command, &r.Name, &r.Arguments, &r.Phase, &r.ExitCode); err != nil {
			t.Fatalf("scan lifecycle request: %v", err)
		}
		requests[r.ID] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate lifecycle requests: %v", err)
	}
	rows.Close()

	for _, r := range requests {
		switch r.Phase {
		case "done":
			if r.ExitCode != 0 {
				t.Errorf("request %s is done with exit code %d", r.ID, r.ExitCode)
			}
		case "failed":
			if r.ExitCode == 0 {
				t.Errorf("request %s failed with exit code 0", r.ID)
			}
		case "cancelled", "interrupted":
		default:
			t.Errorf("request %s has unclassified phase %q after startup recovery", r.ID, r.Phase)
		}
		if err := s.BeginLifecycleRequest(ctx, r.ID, r.Actor); err == nil {
			t.Errorf("request %s began execution after recovery", r.ID)
		}
	}

	fences := make(map[string]string)
	rows, err = s.db.QueryContext(ctx, `SELECT instance_id, request_id FROM cleanup_fences`)
	if err != nil {
		t.Fatalf("query cleanup fences: %v", err)
	}
	for rows.Next() {
		var instanceID, requestID string
		if err := rows.Scan(&instanceID, &requestID); err != nil {
			t.Fatalf("scan cleanup fence: %v", err)
		}
		fences[instanceID] = requestID
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate cleanup fences: %v", err)
	}
	rows.Close()

	receiptInstances := make(map[string]bool)
	rows, err = s.db.QueryContext(ctx, `SELECT request_id, actor, receipt FROM cleanup_receipts`)
	if err != nil {
		t.Fatalf("query cleanup receipts: %v", err)
	}
	for rows.Next() {
		var requestID, actor, payload string
		if err := rows.Scan(&requestID, &actor, &payload); err != nil {
			t.Fatalf("scan cleanup receipt: %v", err)
		}
		var receipt model.CleanupReceipt
		if err := json.Unmarshal([]byte(payload), &receipt); err != nil {
			t.Errorf("unmarshal cleanup receipt %s: %v", requestID, err)
			continue
		}
		inst, ok := byID[receipt.InstanceID]
		if !ok {
			t.Errorf("cleanup receipt %s names missing instance %s", requestID, receipt.InstanceID)
			continue
		}
		if inst.State != model.StateDeleted || inst.DeletedAt == nil {
			t.Errorf("cleanup receipt %s exists but instance %s is %q", requestID, inst.Name, inst.State)
		}
		if _, fenced := fences[inst.ID]; fenced {
			t.Errorf("cleanup receipt %s coexists with a fence on %s", requestID, inst.Name)
		}
		request, ok := requests[requestID]
		if !ok || request.Phase != "done" || request.ExitCode != 0 {
			t.Errorf("cleanup receipt %s lacks a done request: %+v", requestID, request)
		}
		receiptInstances[inst.ID] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate cleanup receipts: %v", err)
	}
	rows.Close()

	for instanceID, requestID := range fences {
		inst, ok := byID[instanceID]
		if !ok {
			t.Errorf("cleanup fence names missing instance %s", instanceID)
			continue
		}
		if inst.State != model.StateDeleting {
			t.Errorf("cleanup fence exists but instance %s is %q", inst.Name, inst.State)
		}
		if request, ok := requests[requestID]; !ok || request.Phase != "interrupted" {
			t.Errorf("cleanup fence %s lacks an interrupted request: %+v", requestID, request)
		}
		if err := s.CheckCleanupFence(ctx, instanceID); err == nil {
			t.Errorf("fence for %s is not enforced", inst.Name)
		}
	}

	imports := make(map[string]string)
	rows, err = s.db.QueryContext(ctx, `SELECT instance_id, phase FROM import_receipts`)
	if err != nil {
		t.Fatalf("query import receipts: %v", err)
	}
	for rows.Next() {
		var instanceID, phase string
		if err := rows.Scan(&instanceID, &phase); err != nil {
			t.Fatalf("scan import receipt: %v", err)
		}
		imports[instanceID] = phase
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate import receipts: %v", err)
	}
	rows.Close()

	for instanceID, phase := range imports {
		inst, exists := byID[instanceID]
		switch phase {
		case "imported":
			if !exists || inst.State != model.StateStopped {
				t.Errorf("imported receipt for %s lacks a stopped instance: %+v", instanceID, inst)
			}
		case "importing":
			if exists && inst.State != model.StateProvisioning {
				t.Errorf("importing receipt for %s has instance state %q", instanceID, inst.State)
			}
		default:
			t.Errorf("import receipt %s has unknown phase %q", instanceID, phase)
		}
	}

	for _, inst := range instances {
		switch inst.State {
		case model.StateDeleting:
			if _, ok := fences[inst.ID]; !ok {
				t.Errorf("instance %s is deleting without a cleanup fence", inst.Name)
			}
		case model.StateDeleted:
			if !receiptInstances[inst.ID] {
				t.Errorf("instance %s is deleted without a cleanup receipt", inst.Name)
			}
		}
	}
}
