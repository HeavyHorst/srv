package provision

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"

	"srv/internal/model"
)

type templateManifest struct {
	Version  int
	ID       string
	Owner    string
	SourceID string
	Image    string
	Kind     string
}

func (p *Provisioner) templatePath(id string, actor model.Actor) (string, error) {
	if !validName.MatchString(id) || strings.TrimSpace(actor.UserLogin) == "" {
		return "", errors.New("template requires a valid srv name and authenticated owner")
	}
	owner := fmt.Sprintf("%x", sha256.Sum256([]byte(strings.ToLower(actor.UserLogin))))
	return filepath.Join(p.cfg.DataDirAbs(), "templates", owner, id), nil
}

func (p *Provisioner) readTemplate(id string, actor model.Actor) (string, templateManifest, error) {
	dir, err := p.templatePath(id, actor)
	var m templateManifest
	if err != nil {
		return "", m, err
	}
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return "", m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return "", m, err
	}
	if m.Version != 1 || m.ID != id || !strings.EqualFold(m.Owner, actor.UserLogin) || m.SourceID == "" || m.Image == "" {
		return "", m, errors.New("invalid template manifest or owner")
	}
	return dir, m, nil
}

// CreateTemplate snapshots a stopped disk, not RAM. The caller must hold the
// instance lifecycle lock and serialize template creation/deletion with Create.
// Existing IDs are immutable: a retry only succeeds for the same instance ID
// and image, and never refreshes the original snapshot.
// Templates are confidential artifacts for the same trust domain as their
// source: cleanup removes active identity files, not arbitrary project secrets
// or recoverable contents in ext4 free blocks/journals. It is not secure erasure.
func (p *Provisioner) CreateTemplate(ctx context.Context, name, templateID string, actor model.Actor) error {
	return p.createTemplate(ctx, name, templateID, actor, false)
}

// VerifyTemplate checks a previously published template without recreating it.
func (p *Provisioner) VerifyTemplate(ctx context.Context, name, templateID string, actor model.Actor) error {
	return p.createTemplate(ctx, name, templateID, actor, true)
}

func (p *Provisioner) createTemplate(ctx context.Context, name, templateID string, actor model.Actor, mustExist bool) error {
	p.admissionMu.Lock()
	defer p.admissionMu.Unlock()
	dest, err := p.templatePath(templateID, actor)
	if err != nil {
		return err
	}
	if err := p.migrationFence(ctx, name); err != nil {
		return err
	}
	inst, err := p.store.GetInstance(ctx, name)
	if err != nil {
		return err
	}
	if !strings.EqualFold(inst.CreatedByUser, actor.UserLogin) {
		return errors.New("template source belongs to another owner")
	}
	if err := p.resolveRuntime(ctx, &inst); err != nil {
		return err
	}
	if inst.State != model.StateStopped || (inst.FirecrackerPID > 0 && processExists(inst.FirecrackerPID)) {
		return errors.New("template source must be stopped")
	}
	if _, old, err := p.readTemplate(templateID, actor); err == nil {
		if old.SourceID == inst.ID && old.Image == inst.ImageName() && (model.Instance{Kind: old.Kind}).KindName() == inst.KindName() {
			info, err := os.Stat(filepath.Join(dest, "rootfs.img"))
			if err != nil {
				return fmt.Errorf("published template disk unavailable: %w", err)
			}
			if !info.Mode().IsRegular() || info.Size() == 0 {
				return errors.New("published template disk is invalid")
			}
			// Also finish a publication whose final directory fsync failed.
			for _, path := range []string{filepath.Dir(dest), filepath.Dir(filepath.Dir(dest)), p.cfg.DataDirAbs()} {
				if err := syncTemplatePath(path); err != nil {
					return err
				}
			}
			return nil
		}
		return errors.New("template ID already belongs to a different source instance or image")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if mustExist {
		return errors.New("published template is missing; refusing to recreate it")
	}
	parent := filepath.Dir(dest)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(parent, ".template-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	disk := filepath.Join(stage, "rootfs.img")
	if err := p.cloneRootFS(ctx, inst.RootFSPath, disk); err != nil {
		return err
	}
	if err := sanitizeTemplateDisk(ctx, disk); err != nil {
		return fmt.Errorf("sanitize template: %w", err)
	}
	m := templateManifest{Version: 1, ID: templateID, Owner: actor.UserLogin, SourceID: inst.ID, Image: inst.ImageName(), Kind: inst.KindName()}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	manifest := filepath.Join(stage, "manifest.json")
	if err := os.WriteFile(manifest, b, 0o400); err != nil {
		return err
	}
	if err := os.Chmod(disk, 0o400); err != nil {
		return err
	}
	for _, path := range []string{disk, manifest, stage} {
		if err := syncTemplatePath(path); err != nil {
			return err
		}
	}
	// A published directory is nonempty, so rename cannot replace an existing ID.
	if err := os.Rename(stage, dest); err != nil {
		return err
	}
	for _, path := range []string{parent, filepath.Dir(parent), p.cfg.DataDirAbs()} {
		if err := syncTemplatePath(path); err != nil {
			return err
		}
	}
	return nil
}

// DeleteTemplate is owner-scoped and idempotent. It never removes a source VM.
func (p *Provisioner) DeleteTemplate(ctx context.Context, templateID string, actor model.Actor) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.admissionMu.Lock()
	defer p.admissionMu.Unlock()
	dir, _, err := p.readTemplate(templateID, actor)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	// Unpublish atomically before reclaiming files; an interrupted recursive
	// removal must not leave a visible template with a missing manifest or disk.
	trash := filepath.Join(filepath.Dir(dir), ".deleted-"+uuid.NewString())
	if err := os.Rename(dir, trash); err != nil {
		return err
	}
	if err := syncTemplatePath(filepath.Dir(dir)); err != nil {
		return err
	}
	if err := os.RemoveAll(trash); err != nil {
		return err
	}
	return syncTemplatePath(filepath.Dir(dir))
}

func syncTemplatePath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}
