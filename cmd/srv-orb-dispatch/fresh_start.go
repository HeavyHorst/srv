package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"

	"srv/internal/model"
)

func orbName(id, previous string) string {
	if previous != "" {
		// Preserve names already published by older dispatchers.
		id += "\x00fresh-start\x00" + previous
	}
	return fmt.Sprintf("orb-%x", sha256.Sum256([]byte(id)))[:60]
}

// Called by spawn under the stable per-Orb lock, after deletion completes.
// Publish the successor before creating it; retries use that same record.
func freshCompute(c config, path string, r record) (record, error) {
	// Older dispatchers also wrote deleted for delete-before-first-spawn.
	// No allocation was attempted, so there is no backend receipt to verify.
	if r.InstanceID == "" && r.CreatedAt == "" && r.BackendID == "" && r.Image == "" && r.RequestID == "" && r.Pending == "" && r.BootstrapGeneration == nil && r.BootstrapFloor == 0 {
		r.Phase = "new"
		return r, save(path, r)
	}
	data, err := os.ReadFile(cleanupPath(path, r))
	if err != nil {
		return r, err
	}
	var proof model.CleanupReceipt
	if err := json.Unmarshal(data, &proof); err != nil {
		return r, err
	}
	if err := validateCleanupReceipt(r, proof); err != nil {
		return r, err
	}
	// Each incarnation keeps its own backend name and cleanup receipt. Older
	// archives may remain on disk, but the current VM does not depend on them.
	next := record{
		OrbID: r.OrbID, Endpoint: c.endpoint, Name: orbName(r.OrbID, r.InstanceID),
		Host: c.host, Phase: "new", PreviousInstanceID: r.InstanceID,
	}
	return next, save(path, next)
}
