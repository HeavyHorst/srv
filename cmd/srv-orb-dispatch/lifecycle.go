package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"time"

	"github.com/google/uuid"
	"srv/internal/model"
	"srv/internal/store"
)

// Initialization is an operator action, never part of provider startup. Losing
// the state directory must not silently create a fresh admission namespace.
func initialize(c config) error {
	if !filepath.IsAbs(c.directory) || c.endpoint == "" {
		return errors.New("absolute state directory and endpoint required")
	}
	if err := os.MkdirAll(c.directory, 0700); err != nil {
		return err
	}
	entries, err := os.ReadDir(c.directory)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return errors.New("init requires an empty state directory; restore existing provider state instead")
	}
	f, err := os.OpenFile(filepath.Join(c.directory, "provider.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.WriteString(c.endpoint); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	dir, err := os.Open(c.directory)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func identity(r record) []string { return []string{r.InstanceID, r.CreatedAt, r.BackendID, r.Image} }

func inspect(ctx context.Context, c config, r record) (*instance, error) {
	data, err := control(ctx, c, r, "inspect", r.Name)
	if err != nil {
		return nil, err
	}
	var response struct{ Instance instance }
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, err
	}
	if response.Instance.Name != r.Name {
		return nil, errors.New("inspect returned a different instance")
	}
	return &response.Instance, nil
}

func matches(r record, v *instance) error {
	if r.InstanceID == "" || r.CreatedAt == "" || r.BackendID == "" || r.Image == "" || v.ID != r.InstanceID || v.CreatedAt != r.CreatedAt || v.BackendID != r.BackendID || v.Kind != "orb" || v.Image != r.Image || v.Imported != "" || v.Migration != "" && v.Migration != "aborted" {
		return errors.New("orb incarnation, backend, or migration binding changed; refusing operation")
	}
	if r.BootstrapGeneration == nil || v.BootstrapGeneration == nil || *v.BootstrapGeneration < r.BootstrapFloor || *v.BootstrapGeneration > *r.BootstrapGeneration || (r.Pending != "bootstrap" && *v.BootstrapGeneration != *r.BootstrapGeneration) {
		return errors.New("bootstrap counter missing or rolled back; refusing reset")
	}
	return nil
}

func operate(ctx context.Context, c config, path string, r *record, operation string) error {
	r.Pending, r.RequestID = operation, uuid.NewString()
	if err := save(path, r); err != nil {
		return err
	}
	args := append([]string{operation, r.Name, "--expect", r.RequestID}, identity(*r)...)
	if _, err := control(ctx, c, *r, args...); err != nil {
		return err
	}
	return resolveOperation(ctx, c, path, r)
}

func resolveOperation(ctx context.Context, c config, path string, r *record) error {
	data, err := control(ctx, c, *r, "operation-result", r.RequestID)
	if err != nil {
		return fmt.Errorf("pending %s remains fenced: %w", r.Pending, err)
	}
	var result store.LifecycleRequest
	if err := json.Unmarshal(data, &result); err != nil {
		return err
	}
	binding := append([]string{r.Pending, r.Name}, identity(*r)...)
	if r.Pending == "bootstrap" {
		if r.BootstrapGeneration == nil {
			return errors.New("bootstrap counter missing")
		}
		binding = append(binding, strconv.FormatInt(*r.BootstrapGeneration, 10))
	}
	expected, _ := json.Marshal(binding)
	if result.ID != r.RequestID || result.Name != r.Name || result.Command != r.Pending || result.Arguments != string(expected) || (result.Phase != "done" && result.Phase != "cancelled") || result.ExitCode != 0 {
		return errors.New("no bound terminal operation receipt; observation alone cannot clear pending execution")
	}
	if r.Pending == "bootstrap" {
		r.BootstrapFloor = *r.BootstrapGeneration
	}
	r.Pending, r.RequestID = "", ""
	return save(path, r)
}

func finishDelete(ctx context.Context, c config, path string, r record) error {
	if r.RequestID == "" || r.InstanceID == "" {
		return errors.New("delete binding missing; operator reconciliation required")
	}
	data, err := control(ctx, c, r, "cleanup-receipt", r.RequestID)
	if err != nil {
		return err
	}
	var proof model.CleanupReceipt
	if err := json.Unmarshal(data, &proof); err != nil {
		return err
	}
	created, err := time.Parse(time.RFC3339Nano, r.CreatedAt)
	if err != nil {
		return err
	}
	if proof.Version != 2 || proof.Kind != "orb" || proof.Image != r.Image || proof.BackendID != r.BackendID || proof.RequestID != r.RequestID || proof.InstanceID != r.InstanceID || proof.Name != r.Name || !proof.CreatedAt.Equal(created) || proof.DeletedAt.IsZero() || proof.AuthKeyDisposition != "never-minted" || proof.RetainedTemplates == nil || !proof.IngressClosed || !proof.RuntimeAbsent || !proof.JailerAbsent || !proof.CgroupAbsent || !proof.NetworkAbsent || !proof.StorageAbsent || !proof.BackupsAbsent || !proof.SnapshotsAbsent {
		return errors.New("incomplete or mismatched cleanup receipt; release remains fenced")
	}
	// Retain the exact authoritative disposition alongside the tombstone.
	proofPath := path + ".cleanup"
	if old, err := os.ReadFile(proofPath); err == nil {
		var prior model.CleanupReceipt
		if json.Unmarshal(old, &prior) != nil || !reflect.DeepEqual(prior, proof) {
			return errors.New("cleanup receipt changed")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := save(proofPath, proof); err != nil {
		return err
	}
	r.Phase = "deleted"
	return save(path, r)
}
