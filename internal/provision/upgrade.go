package provision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"srv/internal/model"
	"srv/internal/storage"
	"srv/internal/vmrunner"
)

// The durable pending journal fences disk consumers across the filesystem/SQL
// boundary. Recovery always restores the retained pre-upgrade pair, never guesses
// which half of a commit completed. Callers hold the instance lifecycle lock.
type upgradeJournal struct {
	InstanceID string
	Directory  string
	Kernel     string
	Initrd     string
	RootFSSize int64
	Pending    bool
	Finalized  bool `json:",omitempty"`
}

func (p *Provisioner) readUpgrade(name string) (upgradeJournal, error) {
	var j upgradeJournal
	dir, err := p.instanceDir(name)
	if err != nil {
		return j, err
	}
	path := filepath.Join(dir, "upgrade.json")
	if err := storage.ValidatePath(path); err != nil {
		return j, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return j, err
	}
	if err := json.Unmarshal(b, &j); err != nil {
		return j, err
	}
	if j.InstanceID == "" || !strings.HasPrefix(j.Directory, "upgrade-") || filepath.Base(j.Directory) != j.Directory {
		return j, errors.New("invalid upgrade journal")
	}
	return j, nil
}

func (p *Provisioner) upgradeFence(name string) error {
	j, err := p.readUpgrade(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if j.Pending {
		return fmt.Errorf("incomplete kernel upgrade; run upgrade %s --rollback", name)
	}
	return nil
}

func writeUpgrade(path string, j upgradeJournal) error {
	b, err := json.Marshal(j)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".upgrade-journal-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(b)
	err = errors.Join(err, f.Sync(), f.Close())
	if err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return storage.SyncPath(filepath.Dir(path))
}

// FinalizeKernelUpgrade discards the rollback disk without touching the running
// guest. Keep boot files: the instance or a retained backup may reference them.
// Callers hold the instance lifecycle lock, as for upgrade and rollback.
func (p *Provisioner) FinalizeKernelUpgrade(ctx context.Context, name string) (model.Instance, error) {
	inst, err := p.store.GetInstance(ctx, name)
	if err != nil {
		return inst, err
	}
	if err := p.migrationOnlyFence(ctx, name); err != nil {
		return inst, err
	}
	if inst.State != model.StateReady && inst.State != model.StateStopped {
		return inst, errors.New("kernel upgrade finalization requires a ready or stopped instance")
	}
	j, err := p.readUpgrade(name)
	if errors.Is(err, os.ErrNotExist) {
		return inst, errors.New("no kernel rollback available")
	}
	if err != nil {
		return inst, err
	}
	if j.InstanceID != inst.ID {
		return inst, errors.New("upgrade belongs to a different instance")
	}
	if j.Pending {
		return inst, errors.New("cannot finalize an incomplete kernel upgrade; run --rollback first")
	}
	dir, err := p.instanceDir(name)
	if err != nil {
		return inst, err
	}
	saved := filepath.Join(dir, j.Directory, "rootfs.img")
	if err := storage.ValidatePath(saved); err != nil {
		return inst, err
	}
	info, err := os.Lstat(saved)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return inst, err
	}
	if err == nil {
		if !info.Mode().IsRegular() {
			return inst, errors.New("rollback disk must be a regular file")
		}
		for _, active := range []string{inst.RootFSPath, inst.KernelPath, inst.InitrdPath} {
			if active == "" {
				continue
			}
			current, err := os.Stat(active)
			// Cold storage can remove the active local disk. An absent path
			// cannot alias the rollback disk and need not be hydrated here.
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return inst, err
			}
			if os.SameFile(info, current) {
				return inst, errors.New("rollback disk is still an active instance artifact")
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return inst, err
	}
	// Persist the tombstone before unlinking. A crash at either boundary leaves
	// rollback unavailable and --finalize able to resume the cleanup.
	if !j.Finalized {
		j.Finalized = true
		if err := writeUpgrade(filepath.Join(dir, "upgrade.json"), j); err != nil {
			return inst, err
		}
		p.recordEvent(inst.ID, "upgrade", "kernel upgrade finalized; rollback discarded", nil)
	} else {
		// A previous attempt may have renamed the tombstone but failed or
		// stopped before syncing its directory. Make it durable before unlink.
		if err := storage.SyncPath(dir); err != nil {
			return inst, err
		}
	}
	if err := os.Remove(saved); err != nil && !errors.Is(err, os.ErrNotExist) {
		return inst, err
	}
	if err := storage.SyncPath(filepath.Dir(saved)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return inst, err
	}
	return inst, nil
}

// UpgradeKernel deliberately does not stop or restart a VM. Readiness failures
// leave the operator a stopped-VM rollback, rather than reverting guest writes
// automatically. Rollback restores the entire pre-upgrade disk (including data).
func (p *Provisioner) UpgradeKernel(ctx context.Context, name string, rollback bool) (model.Instance, error) {
	if err := p.ramStateFence(name); err != nil {
		return model.Instance{}, err
	}
	// Read without reconciliation or the upgrade fence: rollback must remain
	// available after an interrupted commit, for both normal VMs and Orbs.
	inst, err := p.store.GetInstance(ctx, name)
	if err != nil {
		return inst, err
	}
	if err := p.migrationOnlyFence(ctx, name); err != nil {
		return inst, err
	}
	if rollback {
		j, err := p.readUpgrade(name)
		if errors.Is(err, os.ErrNotExist) || (err == nil && j.Finalized) {
			return inst, errors.New("no kernel rollback available")
		}
		if err != nil {
			return inst, err
		}
		if j.InstanceID != inst.ID {
			return inst, errors.New("upgrade belongs to a different instance")
		}
	}
	// Do not reconcile an interrupted upgrade into a startable state.
	if inst.State != model.StateStopped || inst.FirecrackerPID != 0 {
		return inst, errors.New("kernel upgrade/rollback requires a stopped VM")
	}
	proof, ok := p.vmRunner.(interface {
		VerifyInstanceIdle(context.Context, string) error
	})
	if !ok {
		return inst, errors.New("kernel upgrade requires idle-runtime proof")
	}
	if err := proof.VerifyInstanceIdle(ctx, name); err != nil {
		return inst, err
	}
	if err := p.coldDiskFence(ctx, inst, true); err != nil {
		return inst, err
	}
	if rollback {
		j, err := p.readUpgrade(name)
		if err != nil {
			return inst, err
		}
		if j.InstanceID != inst.ID {
			return inst, errors.New("upgrade belongs to a different instance")
		}
		// A crash can leave an unpublished replacement alongside rootfs.img.
		// Reclaim only regular files in our reserved namespace.
		diskDir := filepath.Dir(inst.RootFSPath)
		if err := storage.ValidatePath(diskDir); err != nil {
			return inst, err
		}
		entries, err := os.ReadDir(diskDir)
		if err != nil {
			return inst, err
		}
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), ".upgrade-disk-") {
				continue
			}
			if !entry.Type().IsRegular() {
				return inst, errors.New("nonregular upgrade temporary")
			}
			if err := os.Remove(filepath.Join(diskDir, entry.Name())); err != nil {
				return inst, err
			}
		}
	}
	if err := p.ensureDiskLayout(ctx, &inst); err != nil {
		return inst, err
	}
	if loops, err := loopDevicesForPath(inst.RootFSPath); err != nil || loops != "" {
		return inst, fmt.Errorf("kernel upgrade requires an unattached disk (loops=%q): %v", loops, err)
	}
	if err := p.ensureHostDiskCapacity(ctx, "", 0); err != nil {
		return inst, err
	}
	dir, err := p.instanceDir(name)
	if err != nil {
		return inst, err
	}
	journalPath := filepath.Join(dir, "upgrade.json")
	if rollback {
		j, err := p.readUpgrade(name)
		if err != nil {
			return inst, err
		}
		if j.InstanceID != inst.ID {
			return inst, errors.New("upgrade belongs to a different instance")
		}
		j.Pending = true
		if err := writeUpgrade(journalPath, j); err != nil {
			return inst, err
		}
		saved := filepath.Join(dir, j.Directory)
		if err := replaceUpgradeDisk(ctx, filepath.Join(saved, "rootfs.img"), inst.RootFSPath); err != nil {
			return inst, err
		}
		inst.KernelPath, inst.InitrdPath = j.Kernel, j.Initrd
		inst.RootFSSizeBytes = j.RootFSSize
		inst.UpdatedAt = time.Now().UTC()
		if err := p.store.UpdateInstance(ctx, inst); err != nil {
			return inst, err
		}
		// Keep the rollback record repeatable, including after an ambiguous reply.
		j.Pending = false
		if err := writeUpgrade(journalPath, j); err != nil {
			return inst, err
		}
		p.recordEvent(inst.ID, "upgrade", "kernel upgrade rolled back", nil)
		return inst, nil
	}
	if err := p.upgradeFence(name); err != nil {
		return inst, err
	}
	image, err := p.cfg.ResolveImage(inst.ImageName())
	if err != nil {
		return inst, err
	}
	if image.Initrd != "" {
		return inst, errors.New("kernel upgrade currently requires an initrd-free base image")
	}
	// Ask the running helper: its allowed roots and legacy pair may have been
	// overridden independently of the control-plane configuration.
	resolver, ok := p.vmRunner.(interface {
		ResolveBootArtifacts(context.Context, vmrunner.StartRequest) (vmrunner.BootFiles, error)
	})
	if !ok {
		return inst, errors.New("kernel upgrade requires runner boot-artifact resolution")
	}
	boots := []vmrunner.StartRequest{
		{Name: name, KernelPath: image.Kernel, InitrdPath: image.Initrd},
		{Name: name, KernelPath: inst.KernelPath, InitrdPath: inst.InitrdPath},
	}
	for i, request := range boots {
		selected, err := resolver.ResolveBootArtifacts(ctx, request)
		if err != nil {
			return inst, err
		}
		boots[i].KernelPath, boots[i].InitrdPath = selected.KernelPath, selected.InitrdPath
	}
	stage, err := os.MkdirTemp(dir, "upgrade-")
	if err != nil {
		return inst, err
	}
	// Unpublished stages are safe to remove. After publication they are recovery
	// material even if a sync or database operation returns an ambiguous error.
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(stage)
		}
	}()
	for _, pair := range [][2]string{{image.RootFS, "base.img"}, {boots[0].KernelPath, "vmlinux"}, {inst.RootFSPath, "rootfs.img"}, {boots[1].KernelPath, "old-vmlinux"}} {
		if err := cloneUpgradeFile(ctx, pair[0], filepath.Join(stage, pair[1]), strings.HasSuffix(pair[1], ".img")); err != nil {
			return inst, err
		}
	}
	manifest, err := os.ReadFile(filepath.Join(filepath.Dir(image.RootFS), "manifest.txt"))
	if err != nil {
		return inst, fmt.Errorf("read paired build manifest: %w", err)
	}
	for _, pair := range [][2]string{{"base.img", filepath.Base(image.RootFS)}, {"vmlinux", filepath.Base(image.Kernel)}} {
		digest, err := diskDigest(ctx, filepath.Join(stage, pair[0]))
		if err != nil {
			return inst, err
		}
		if !strings.Contains("\n"+string(manifest), "\n"+digest+"  "+pair[1]+"\n") {
			return inst, fmt.Errorf("build manifest does not bind %s; rebuild the base image", pair[1])
		}
	}
	j := upgradeJournal{InstanceID: inst.ID, Directory: filepath.Base(stage), Kernel: filepath.Join(stage, "old-vmlinux"), RootFSSize: inst.RootFSSizeBytes, Pending: true}
	if inst.InitrdPath != "" {
		j.Initrd = filepath.Join(stage, "old-initrd")
		if err := cloneUpgradeFile(ctx, boots[1].InitrdPath, j.Initrd, false); err != nil {
			return inst, err
		}
	}
	// Source authorization does not imply that the helper accepts the private
	// destination paths (for example, with a different instances-dir mount).
	for _, request := range []vmrunner.StartRequest{
		{Name: name, KernelPath: filepath.Join(stage, "vmlinux")},
		{Name: name, KernelPath: j.Kernel, InitrdPath: j.Initrd},
	} {
		if _, err := resolver.ResolveBootArtifacts(ctx, request); err != nil {
			return inst, fmt.Errorf("authorize private boot pair: %w", err)
		}
	}
	prepared := filepath.Join(stage, "prepared.img")
	if err := cloneUpgradeFile(ctx, inst.RootFSPath, prepared, true); err != nil {
		return inst, err
	}
	if err := upgradeModules(ctx, filepath.Join(stage, "base.img"), prepared, stage); err != nil {
		return inst, err
	}
	if err := storage.SyncPath(prepared); err != nil {
		return inst, err
	}
	if err := storage.SyncPath(stage); err != nil {
		return inst, err
	}
	published = true
	if err := writeUpgrade(journalPath, j); err != nil {
		return inst, err
	}
	if err := replaceUpgradeDisk(ctx, prepared, inst.RootFSPath); err != nil {
		return inst, err
	}
	inst.KernelPath, inst.InitrdPath = filepath.Join(stage, "vmlinux"), ""
	inst.UpdatedAt = time.Now().UTC()
	if err := p.store.UpdateInstance(ctx, inst); err != nil {
		return inst, err
	}
	j.Pending = false
	if err := writeUpgrade(journalPath, j); err != nil {
		return inst, err
	}
	for _, path := range []string{"base.img", "prepared.img", "module"} {
		_ = os.RemoveAll(filepath.Join(stage, path))
	}
	p.recordEvent(inst.ID, "upgrade", "kernel and modules upgraded; pre-upgrade disk retained", nil)
	return inst, nil
}

func cloneUpgradeFile(ctx context.Context, src, dest string, disk bool) error {
	if err := storage.ValidatePath(src); err != nil {
		return err
	}
	if disk {
		if err := reflinkCloneFile(ctx, src, dest); err != nil {
			return err
		}
	} else {
		// Boot artifacts may live outside the reflink-capable disk filesystem.
		info, err := os.Stat(src)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("boot artifact %q must be a regular file", src)
		}
		if output, err := exec.CommandContext(ctx, "cp", "--reflink=auto", src, dest).CombinedOutput(); err != nil {
			return fmt.Errorf("copy boot artifact: %w: %s", err, strings.TrimSpace(string(output)))
		}
	}
	if err := os.Chmod(dest, 0o660); err != nil {
		return err
	}
	return storage.SyncPath(dest)
}

func replaceUpgradeDisk(ctx context.Context, source, dest string) error {
	// Never reuse a pathname: a copy child may outlive a crashed parent.
	// Its private temporary must not alias a subsequent recovery's copy.
	f, err := os.CreateTemp(filepath.Dir(dest), ".upgrade-disk-")
	if err != nil {
		return err
	}
	_ = f.Close()
	defer os.Remove(f.Name())
	if err := cloneUpgradeFile(ctx, source, f.Name(), true); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), dest); err != nil {
		return err
	}
	return storage.SyncPath(filepath.Dir(dest))
}

// Work only on private copies, using userspace ext4 tools, never host mounts.
func upgradeModules(ctx context.Context, base, disk, stage string) error {
	if err := runRootFSCheckContext(ctx, disk, "check upgrade disk"); err != nil {
		return err
	}
	src := templateDisk{ctx: ctx, path: base}
	dst := templateDisk{ctx: ctx, path: disk, remaining: 100000}
	// Both supported distributions use merged /usr. clean rejects symlinked
	// ancestors in the untrusted guest before any path is traversed for writes.
	if err := dst.clean("/usr/lib/modules"); err != nil {
		return err
	}
	if strings.ContainsAny(stage, "\x00\r\n\"\\") {
		return errors.New("unsupported upgrade staging path")
	}
	// dump without -p avoids rdump's privileged host chown. write creates
	// root-owned guest files even when the control plane runs as the srv user.
	tmp := filepath.Join(stage, "module")
	count := 0
	remaining := 100000
	var copyDirectory func(string, int) error
	copyDirectory = func(dir string, depth int) error {
		if depth > 64 {
			return errors.New("module tree exceeds traversal limit")
		}
		entries, err := src.list(dir)
		if err != nil {
			return err
		}
		if _, err := dst.command("mkdir", dir); err != nil {
			return err
		}
		for _, entry := range entries {
			remaining--
			guest := dir + "/" + entry.name
			if remaining < 0 || strings.ContainsAny(guest, "\x00\r\n\"\\") {
				return errors.New("unsupported module tree")
			}
			switch entry.mode & 0170000 {
			case 0040000:
				if err := copyDirectory(guest, depth+1); err != nil {
					return err
				}
			case 0100000:
				if _, err := src.run("dump \"" + guest + "\" \"" + tmp + "\""); err != nil {
					return err
				}
				if _, err := dst.run("write \"" + tmp + "\" \"" + guest + "\""); err != nil {
					return err
				}
				if strings.Contains(entry.name, ".ko") {
					count++
				}
			default:
				return fmt.Errorf("nonregular module file: %s", guest)
			}
		}
		return nil
	}
	if err := copyDirectory("/usr/lib/modules", 0); err != nil {
		return err
	}
	if count == 0 {
		return errors.New("base image contains no kernel modules")
	}
	return runRootFSCheckContext(ctx, disk, "check upgraded filesystem")
}
