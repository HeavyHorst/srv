package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type snapshotRecord struct {
	ID       string `json:"snapshot_id"`
	Name     string `json:"name"`
	Endpoint string `json:"endpoint"`
	Host     string `json:"host"`
	Image    string `json:"image"`
	Source   string `json:"source_orb"`
	Phase    string `json:"phase"`
}

func snapshotName(id string) string {
	return fmt.Sprintf("project-%x", sha256.Sum256([]byte(id)))[:60]
}

func snapshotPath(c config, id string) string {
	return filepath.Join(c.directory, snapshotName(id)+".json")
}

func lockSnapshot(ctx context.Context, c config, id string) (func(), error) {
	f, err := os.OpenFile(snapshotPath(c, id)+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() { _ = f.Close() }, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			break
		}
		if err = wait(ctx); err != nil {
			break
		}
	}
	_ = f.Close()
	return nil, err
}

func loadSnapshot(c config, id string) (*snapshotRecord, error) {
	b, err := os.ReadFile(snapshotPath(c, id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil // Amp permits a base-image fallback for unknown snapshots.
	}
	if err != nil {
		return nil, err
	}
	var s snapshotRecord
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	if s.ID != id || s.Name != snapshotName(id) || s.Endpoint != c.endpoint || s.Source == "" || s.Image == "" || (s.Phase != "creating" && s.Phase != "ready") {
		return nil, errors.New("invalid snapshot mapping or endpoint mismatch")
	}
	return &s, nil
}

func captureSnapshot(ctx context.Context, c config, j job, r record, image string, s *snapshotRecord) error {
	if image == "" {
		return errors.New("snapshot requires srv inventory with an image name")
	}
	if s == nil {
		s = &snapshotRecord{ID: j.snapshot, Name: snapshotName(j.snapshot), Endpoint: c.endpoint, Host: r.Host, Image: image, Source: j.id, Phase: "creating"}
		if err := save(snapshotPath(c, j.snapshot), s); err != nil {
			return err
		}
	} else if s.Source != j.id || s.Host != r.Host || s.Image != image {
		return errors.New("snapshot ID already belongs to another source, host or image")
	}
	operation := "create"
	if s.Phase == "ready" {
		operation = "verify" // Never recreate a lost published template from newer work.
	}
	if _, err := control(ctx, c, r, "project-template", operation, r.Name, s.Name); err != nil {
		return err
	}
	s.Phase = "ready"
	return save(snapshotPath(c, j.snapshot), s)
}
