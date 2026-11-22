package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type snapshotRecord struct {
	ID               string     `json:"snapshot_id"`
	Name             string     `json:"name"`
	Endpoint         string     `json:"endpoint"`
	Host             string     `json:"host"`
	Image            string     `json:"image"`
	Source           string     `json:"source_orb"`
	SourceInstanceID string     `json:"source_instance_id,omitempty"`
	Phase            string     `json:"phase"`
	PublishedAt      *time.Time `json:"published_at,omitempty"`
	LastUsedAt       *time.Time `json:"last_used_at,omitempty"`
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
	return parseSnapshot(c, snapshotPath(c, id), id, b)
}

func parseSnapshot(c config, path, id string, b []byte) (*snapshotRecord, error) {
	var s snapshotRecord
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	if s.ID == "" || (id != "" && s.ID != id) || path != snapshotPath(c, s.ID) || s.Name != snapshotName(s.ID) || s.Endpoint != c.endpoint || s.Source == "" || s.Image == "" || (s.Phase != "creating" && s.Phase != "ready") {
		return nil, errors.New("invalid snapshot mapping or endpoint mismatch")
	}
	return &s, nil
}

func checkSnapshotGCState(c config) error {
	if !filepath.IsAbs(c.directory) || c.endpoint == "" || strings.HasPrefix(c.endpoint, "-") {
		return errors.New("set an absolute SRV_ORB_STATE_DIR and a valid SRV_ORB_ENDPOINT")
	}
	marker, err := os.ReadFile(filepath.Join(c.directory, "provider.json"))
	if err != nil || string(marker) != c.endpoint {
		return errors.New("provider state missing or endpoint changed")
	}
	return nil
}

func snapshotGCDryRun(c config, cutoff time.Time, out io.Writer) error {
	mappings, err := scanSnapshotMappings(c)
	if err != nil {
		return err
	}
	type candidate struct {
		SnapshotID       string     `json:"snapshot_id"`
		Template         string     `json:"template"`
		Host             string     `json:"host"`
		MappingUpdatedAt time.Time  `json:"mapping_updated_at"`
		PublishedAt      *time.Time `json:"published_at"`
		LastUsedAt       *time.Time `json:"last_used_at"`
	}
	candidates := []candidate{}
	for _, mapping := range mappings {
		s := mapping.snapshot
		if s.Phase == "ready" && mapping.modified.Before(cutoff) && (s.LastUsedAt == nil || s.LastUsedAt.Before(cutoff)) {
			candidates = append(candidates, candidate{s.ID, s.Name, s.Host, mapping.modified, s.PublishedAt, s.LastUsedAt})
		}
	}
	return json.NewEncoder(out).Encode(struct {
		DryRun     bool        `json:"dry_run"`
		Cutoff     time.Time   `json:"mapping_updated_before"`
		Candidates []candidate `json:"candidates"`
	}{true, cutoff, candidates})
}

type snapshotMapping struct {
	snapshot *snapshotRecord
	modified time.Time
}

func readSnapshotMapping(c config, path string) (snapshotMapping, error) {
	f, err := os.Open(path)
	if err != nil {
		return snapshotMapping{}, err
	}
	defer f.Close()
	// State is replaced atomically. Read its contents and timestamp from one inode.
	info, err := f.Stat()
	if err != nil {
		return snapshotMapping{}, err
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return snapshotMapping{}, err
	}
	s, err := parseSnapshot(c, path, "", b)
	return snapshotMapping{s, info.ModTime().UTC()}, err
}

func scanSnapshotMappings(c config) ([]snapshotMapping, error) {
	if err := checkSnapshotGCState(c); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(c.directory)
	if err != nil {
		return nil, err
	}
	var mappings []snapshotMapping
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "project-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		path := filepath.Join(c.directory, name)
		if !entry.Type().IsRegular() {
			return nil, fmt.Errorf("%s is not a regular snapshot mapping", name)
		}
		mapping, err := readSnapshotMapping(c, path)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		mappings = append(mappings, mapping)
	}
	return mappings, nil
}

func captureSnapshot(ctx context.Context, c config, j job, r record, image string, s *snapshotRecord) error {
	if image == "" {
		return errors.New("snapshot requires srv inventory with an image name")
	}
	if s == nil {
		s = &snapshotRecord{ID: j.snapshot, Name: snapshotName(j.snapshot), Endpoint: c.endpoint, Host: r.Host, Image: image, Source: j.id, SourceInstanceID: r.InstanceID, Phase: "creating"}
		if err := save(snapshotPath(c, j.snapshot), s); err != nil {
			return err
		}
	} else if s.Source != j.id || s.Host != r.Host || s.Image != image {
		return errors.New("snapshot ID already belongs to another source, host or image")
	}
	// Old mappings may be completed only by the original, non-replacement
	// incarnation. Consuming a ready template is independent of authorship.
	if s.SourceInstanceID != r.InstanceID && (s.SourceInstanceID != "" || r.PreviousInstanceID != "") {
		return errors.New("snapshot ID belongs to another source incarnation")
	}
	operation := "create"
	if s.Phase == "ready" {
		operation = "verify" // Never recreate a lost published template from newer work.
	}
	if _, err := control(ctx, c, r, "project-template", operation, r.Name, s.Name); err != nil {
		return err
	}
	if s.Phase == "creating" {
		now := time.Now().UTC()
		s.PublishedAt = &now
	}
	s.SourceInstanceID = r.InstanceID
	s.Phase = "ready"
	return save(snapshotPath(c, j.snapshot), s)
}
