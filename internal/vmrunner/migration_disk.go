package vmrunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"syscall"

	"srv/internal/coldstore"
	"srv/internal/storage"
)

// MigrationDisk describes immutable transfer objects, not a space reservation.
// Base is retained at the destination; Disk is discarded after publication.
type MigrationDisk struct {
	Disk coldstore.Object  `json:"disk"`
	Base *coldstore.Object `json:"base,omitempty"`
}

type MigrationDiskRequest struct {
	Action       string                    `json:"action"`
	Name         string                    `json:"name"`
	InstanceID   string                    `json:"instance_id"`
	Token        string                    `json:"token"`
	ExpectedDisk storage.SubvolumeIdentity `json:"expected_disk"`
	Transfer     MigrationDisk             `json:"transfer"`
}

type MigrationDiskResult struct {
	Transfer          MigrationDisk `json:"transfer"`
	BasePresent       bool          `json:"base_present"`
	SerializedPayload bool          `json:"serialized_payload"`
}

type migrationDiskReceipt struct {
	InstanceID string                    `json:"instance_id"`
	Transfer   MigrationDisk             `json:"transfer"`
	LocalDisk  storage.SubvolumeIdentity `json:"local_disk"`
}

// MigrationDisk is an internal streaming RPC. Only the trusted frontend's
// migration path may supply its input through srv; public import never does.
func (c *Client) MigrationDisk(ctx context.Context, q MigrationDiskRequest, in io.Reader, out io.Writer) (MigrationDiskResult, error) {
	var result MigrationDiskResult
	metadata, err := json.Marshal(q)
	if err != nil {
		return result, err
	}
	if in != nil {
		// HTTP owns its request body, not the enclosing SSH session. Closing
		// that session here would discard the command's exit status.
		in = io.NopCloser(in)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/vm/migration-disk-v1", in)
	if err != nil {
		return result, err
	}
	req.Header.Set("X-Srv-Migration", string(metadata))
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return result, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return result, fmt.Errorf("native migration runner: %s: %s", resp.Status, body)
	}
	if out != nil {
		_, err = io.Copy(out, resp.Body)
	} else {
		err = json.NewDecoder(io.LimitReader(resp.Body, 8192)).Decode(&result)
	}
	if err == nil && q.Action == "check" && !result.SerializedPayload {
		err = errors.New("upgrade runner: native receive requires serialized payload admission")
	}
	return result, err
}

func (s *Server) migrationSource(q MigrationDiskRequest) string {
	return filepath.Join(s.config.MigrationStoreDir, "sources", receiptKey(q.Name), q.Token)
}

func (s *Server) migrationImportPath(name string) string {
	// Flat *.json files in receiptDir are exclusively lifecycle start receipts.
	return filepath.Join(s.receiptDir(name), "migration-import")
}

func (s *Server) handleMigrationDisk(w http.ResponseWriter, r *http.Request) {
	var q MigrationDiskRequest
	metadata := r.Header.Get("X-Srv-Migration")
	if r.Method != http.MethodPost || len(metadata) > 8192 || json.Unmarshal([]byte(metadata), &q) != nil ||
		!validName.MatchString(q.Name) || !canonicalColdUUID(q.InstanceID) || !canonicalColdUUID(q.Token) {
		respondError(w, http.StatusBadRequest, errors.New("invalid native migration request"))
		return
	}
	if s.config.MigrationStoreDir == "" {
		respondError(w, http.StatusConflict, errors.New("configure SRV_MIGRATION_STORE on both hosts before native migration"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), coldstore.DefaultTimeout)
	defer cancel()
	if q.Action == "receive-base" || q.Action == "receive-disk" {
		var release func()
		var err error
		ctx, release, err = storage.AcquirePayload(ctx, filepath.Dir(s.config.InstancesDir))
		if err != nil {
			respondError(w, http.StatusConflict, err)
			return
		}
		defer release()
	}
	// Match hydration's payload-before-admission order so drain cannot
	// deadlock a deletion that owns payload and still needs the runner.
	s.startAdmission.RLock()
	defer s.startAdmission.RUnlock()
	if s.draining {
		respondError(w, http.StatusServiceUnavailable, errors.New("runner is draining"))
		return
	}
	ctx, unlock, err := s.lockNameInherited(ctx, q.Name)
	if err != nil {
		respondError(w, http.StatusConflict, err)
		return
	}
	defer unlock()
	if err := coldstore.ValidateRoot(s.config.MigrationStoreDir); err != nil {
		respondError(w, http.StatusConflict, err)
		return
	}
	storeFS, err := storage.FilesystemUUID(s.config.MigrationStoreDir)
	if err != nil {
		respondError(w, http.StatusConflict, err)
		return
	}
	instancesFS, err := storage.FilesystemUUID(s.config.InstancesDir)
	if err != nil {
		respondError(w, http.StatusConflict, err)
		return
	}
	if storeFS != instancesFS {
		respondError(w, http.StatusConflict, errors.New("migration store and instances must be on the same btrfs filesystem"))
		return
	}
	// Capability checks precede stopping/fencing the source and are valid while
	// it is running. Every operation that touches its disk still requires idle.
	if q.Action == "check" {
		_ = json.NewEncoder(w).Encode(MigrationDiskResult{SerializedPayload: true})
		return
	}
	if err = s.storageIdleLocked(q.Name); err != nil {
		respondError(w, http.StatusConflict, err)
		return
	}
	result, stream, err := s.migrationDiskLocked(ctx, q, r.Body)
	if err != nil {
		respondError(w, http.StatusConflict, err)
		return
	}
	if stream != nil {
		// Announce success only when the producer has started. A later failure
		// aborts HTTP rather than turning an incomplete stream into success.
		if err := stream(w); err != nil {
			panic(http.ErrAbortHandler)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

func (s *Server) migrationDiskLocked(ctx context.Context, q MigrationDiskRequest, in io.Reader) (MigrationDiskResult, func(io.Writer) error, error) {
	var result MigrationDiskResult
	root := s.config.MigrationStoreDir
	disk := filepath.Join(s.config.InstancesDir, q.Name, "disk")
	source := s.migrationSource(q)
	if q.Action == "prepare" {
		id, err := storage.ValidateDiskSubvolume(ctx, disk)
		if err != nil || id != q.ExpectedDisk {
			return result, nil, errors.New("source disk identity changed")
		}
		cold, err := s.readCold(ColdRequest{Name: q.Name, InstanceID: q.InstanceID})
		if err != nil {
			return result, nil, err
		}
		if cold != nil && cold.Phase != "hot" {
			return result, nil, errors.New("hydrate cold disk before migration")
		}
		if b, err := os.ReadFile(filepath.Join(source, "manifest.json")); err == nil {
			var receipt migrationDiskReceipt
			if json.Unmarshal(b, &receipt) != nil || receipt.InstanceID != q.InstanceID || receipt.LocalDisk != id || receipt.Transfer.Disk.ID != q.Token {
				return result, nil, errors.New("migration snapshot belongs to a different incarnation")
			}
			result.Transfer = receipt.Transfer
			return result, nil, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return result, nil, err
		}
		if err := os.MkdirAll(source, 0700); err != nil {
			return result, nil, err
		}
		path := filepath.Join(source, "disk")
		// An interrupted snapshot without its manifest has no incarnation proof.
		// Never silently adopt it; retain it for explicit recovery.
		if err := storage.SnapshotSubvolume(ctx, disk, path, true); err != nil {
			return result, nil, err
		}
		object, err := coldObject(ctx, q.Token, path)
		if err != nil {
			return result, nil, err
		}
		result.Transfer.Disk = object
		baseName, baseID, err := s.findColdBase(ctx, id.ParentUUID)
		if err != nil {
			return result, nil, err
		}
		if baseName != "" {
			base, err := coldObject(ctx, baseID.UUID, filepath.Join(filepath.Dir(s.config.InstancesDir), "image-bases", baseName, "disk"))
			if err != nil {
				return result, nil, err
			}
			result.Transfer.Base = &base
		} else {
			// A VM already migrated here records its original common base,
			// whose UUID is no longer its immediate writable snapshot parent.
			b, err := os.ReadFile(s.migrationImportPath(q.Name))
			if err == nil {
				var receipt migrationDiskReceipt
				if json.Unmarshal(b, &receipt) != nil {
					return result, nil, errors.New("invalid migration provenance")
				}
				if receipt.InstanceID == q.InstanceID && receipt.LocalDisk == id && receipt.Transfer.Base != nil {
					base, err := coldstore.LocalStat(ctx, root, receipt.Transfer.Base.ID)
					if err != nil || base != *receipt.Transfer.Base {
						return result, nil, errors.New("migration base is missing or changed")
					}
					result.Transfer.Base = &base
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return result, nil, err
			}
		}
		if result.Transfer.Base != nil {
			result.Transfer.Disk.Parent = result.Transfer.Base.ID
		}
		if err := atomicColdJSON(filepath.Join(source, "manifest.json"), migrationDiskReceipt{InstanceID: q.InstanceID, Transfer: result.Transfer, LocalDisk: id}, 0600); err != nil {
			return result, nil, err
		}
		return result, nil, nil
	}
	if q.Action == "send-base" || q.Action == "send-disk" || q.Action == "cleanup" {
		var receipt migrationDiskReceipt
		b, err := os.ReadFile(filepath.Join(source, "manifest.json"))
		if err != nil {
			return result, nil, err
		}
		if json.Unmarshal(b, &receipt) != nil || receipt.InstanceID != q.InstanceID {
			return result, nil, errors.New("migration source receipt mismatch")
		}
		if q.Action == "cleanup" {
			path := filepath.Join(source, "disk")
			id, err := storage.InspectSubvolume(ctx, path)
			if err != nil {
				return result, nil, err
			}
			if id.UUID != receipt.Transfer.Disk.UUID {
				return result, nil, errors.New("migration snapshot identity changed")
			}
			if err := storage.DeleteSubvolume(ctx, path, id); err != nil {
				return result, nil, err
			}
			return result, nil, os.RemoveAll(source)
		}
		parent := ""
		if receipt.Transfer.Base != nil {
			base := *receipt.Transfer.Base
			name, _, err := s.findColdBase(ctx, base.UUID)
			if err != nil {
				return result, nil, err
			}
			if name != "" {
				parent = filepath.Join(filepath.Dir(s.config.InstancesDir), "image-bases", name, "disk")
			} else {
				parent = filepath.Join(root, "objects", base.ID, "disk")
			}
		}
		object, path := receipt.Transfer.Disk, filepath.Join(source, "disk")
		if q.Action == "send-base" {
			if receipt.Transfer.Base == nil {
				return result, nil, errors.New("migration has no base")
			}
			object, path, parent = *receipt.Transfer.Base, parent, ""
		}
		return result, func(out io.Writer) error { return coldstore.Send(ctx, object, path, parent, out) }, nil
	}
	transfer := q.Transfer
	if transfer.Disk.ID != q.Token || transfer.Disk.Parent != "" && (transfer.Base == nil || transfer.Disk.Parent != transfer.Base.ID) {
		return result, nil, errors.New("invalid migration objects")
	}
	switch q.Action {
	case "base-present":
		if transfer.Base == nil {
			result.BasePresent = true
			return result, nil, nil
		}
		base, err := coldstore.LocalStat(ctx, root, transfer.Base.ID)
		if errors.Is(err, os.ErrNotExist) {
			return result, nil, nil
		}
		if err != nil {
			return result, nil, err
		}
		if base != *transfer.Base {
			return result, nil, errors.New("existing base differs from source")
		}
		result.BasePresent = true
	case "receive-base", "receive-disk":
		object := transfer.Disk
		if q.Action == "receive-base" {
			if transfer.Base == nil {
				return result, nil, errors.New("migration has no base")
			}
			object = *transfer.Base
		}
		if err := storage.CheckDiskSpace(root, object.Size); err != nil {
			return result, nil, err
		}
		return result, nil, coldstore.Receive(ctx, root, object, in)
	case "publish":
		if err := s.coldDeletionFence(ColdRequest{Name: q.Name, InstanceID: q.InstanceID}); err != nil {
			return result, nil, err
		}
		if err := storage.ValidatePath(disk); err != nil {
			return result, nil, err
		}
		info, err := os.Stat(filepath.Dir(disk))
		if err != nil {
			return result, nil, err
		}
		if err := coldstore.Clone(ctx, root, transfer.Disk, disk); err != nil {
			return result, nil, err
		}
		owner := info.Sys().(*syscall.Stat_t)
		for _, path := range []string{disk, filepath.Join(disk, "rootfs.img")} {
			if err := os.Chown(path, int(owner.Uid), int(owner.Gid)); err != nil {
				return result, nil, err
			}
		}
		id, err := storage.ValidateDiskSubvolume(ctx, disk)
		if err != nil {
			return result, nil, err
		}
		if err := atomicColdJSON(s.migrationImportPath(q.Name), migrationDiskReceipt{InstanceID: q.InstanceID, Transfer: transfer, LocalDisk: id}, 0600); err != nil {
			return result, nil, err
		}
		return result, nil, storage.SyncPath(filepath.Dir(disk))
	case "discard":
		return result, nil, coldstore.Remove(ctx, root, transfer.Disk.ID)
	default:
		return result, nil, errors.New("unknown migration disk action")
	}
	return result, nil, nil
}
