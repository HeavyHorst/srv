package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"

	"github.com/google/uuid"
	"srv/internal/model"
)

func orbName(id, previous string) string {
	if previous != "" {
		id += "\x00fresh-start\x00" + previous
	}
	return fmt.Sprintf("orb-%x", sha256.Sum256([]byte(id)))[:60]
}

// Every successor has its own backend name and cleanup file. The root record
// selects the current incarnation; archived retired records link its history.
func readRetired(c config, path, orbID, id string) (record, error) {
	var r record
	if _, err := uuid.Parse(id); err != nil {
		return r, errors.New("invalid retired instance UUID")
	}
	data, err := os.ReadFile(path + ".retired-" + id)
	if err != nil {
		return r, fmt.Errorf("retired predecessor unavailable: %w", err)
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return r, err
	}
	if r.OrbID != orbID || r.Endpoint != c.endpoint || r.InstanceID != id || r.Name != orbName(orbID, r.PreviousInstanceID) {
		return r, errors.New("retired predecessor identity mismatch")
	}
	return r, verifyRetirement(path, r)
}

func verifyRetirement(path string, r record) error {
	if r.Phase != "retired" || r.BootstrapGeneration == nil || r.BootstrapFloor < 0 || *r.BootstrapGeneration < r.BootstrapFloor {
		return errors.New("fresh start requires a completed, bound retirement")
	}
	data, err := os.ReadFile(cleanupPath(path, r))
	if err != nil {
		return err
	}
	var proof model.CleanupReceipt
	if err := json.Unmarshal(data, &proof); err != nil {
		return err
	}
	return validateCleanupReceipt(r, proof)
}

// Called under the stable per-Orb lock. No backend command or token is needed:
// only a later Amp spawn can allocate and bootstrap the authorized workspace.
func authorizeFreshStart(c config, path string, j job, r record) error {
	// Replaying any prior authorization must never reset a successor's disk,
	// including after that successor has itself been retired or deleted.
	seen := map[string]bool{}
	for previous := r.PreviousInstanceID; previous != ""; {
		if seen[previous] {
			return errors.New("cyclic retirement history")
		}
		seen[previous] = true
		prior, err := readRetired(c, path, j.id, previous)
		if err != nil {
			return err
		}
		if previous == j.expected {
			// Re-persist the unchanged current record: a previous publication
			// may have renamed successfully but failed its directory fsync.
			return save(path, r)
		}
		previous = prior.PreviousInstanceID
	}
	if r.InstanceID != j.expected {
		return errors.New("fresh-start retired instance UUID mismatch")
	}
	if err := verifyRetirement(path, r); err != nil {
		return err
	}
	archive := path + ".retired-" + r.InstanceID
	if data, err := os.ReadFile(archive); err == nil {
		var prior record
		if json.Unmarshal(data, &prior) != nil || !reflect.DeepEqual(prior, r) {
			return errors.New("retired record changed; refusing to overwrite history")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	} else if err := save(archive, r); err != nil {
		return err
	}
	// Archive is durable before publishing the successor. A crash before this
	// save leaves the old retirement current; a retry reuses the same archive.
	return save(path, record{
		OrbID: j.id, Endpoint: c.endpoint, Name: orbName(j.id, r.InstanceID),
		Host: c.host, Phase: "new", PreviousInstanceID: r.InstanceID,
	})
}
