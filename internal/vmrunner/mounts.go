package vmrunner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	"golang.org/x/sys/unix"
	"srv/internal/storage"
)

// Hard links cannot cross btrfs subvolumes. Expose pinned files before the
// jailer starts, so its private mount namespace inherits all guest artifacts.
type bindChrootStrategy struct{}

func (bindChrootStrategy) AdaptHandlers(handlers *firecracker.Handlers) error {
	if !handlers.FcInit.Has(firecracker.CreateLogFilesHandlerName) {
		return firecracker.ErrRequiredHandlerMissing
	}
	handlers.FcInit = handlers.FcInit.Prepend(firecracker.Handler{
		Name: firecracker.LinkFilesToRootFSHandlerName,
		Fn: func(_ context.Context, m *firecracker.Machine) error {
			j := m.Cfg.JailerCfg
			if j == nil {
				return firecracker.ErrMissingJailerConfig
			}
			paths, err := resolveJailerRuntimePaths(j.ChrootBaseDir, j.ExecFile, j.ID)
			if err != nil {
				return err
			}
			for _, artifact := range []struct {
				path *string
				name string
			}{{&m.Cfg.KernelImagePath, "kernel"}, {&m.Cfg.InitrdPath, "initrd"}} {
				if *artifact.path == "" {
					continue
				}
				if err := bindFileIntoJail(*artifact.path, filepath.Join(paths.RootDir, artifact.name), true); err != nil {
					return err
				}
				*artifact.path = artifact.name
			}
			for i := range m.Cfg.Drives {
				drive := &m.Cfg.Drives[i]
				name := fmt.Sprintf("drive-%d", i)
				if err := bindFileIntoJail(firecracker.StringValue(drive.PathOnHost), filepath.Join(paths.RootDir, name), firecracker.BoolValue(drive.IsReadOnly)); err != nil {
					return err
				}
				drive.PathOnHost = firecracker.String(name)
			}
			return nil
		},
	})
	return nil
}

func bindFileIntoJail(source, target string, readonly bool) error {
	// Resolve no symlinks, including parent components, and pin the inode until
	// mount completes. A renamed/replaced source path cannot change what we mount.
	f, err := storage.OpenNoSymlinks(source, unix.O_PATH)
	if err != nil {
		return fmt.Errorf("open jail source %s: %w", source, err)
	}
	defer f.Close()
	fd := int(f.Fd())
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("jail source %s is not a regular file", source)
	}
	parent := filepath.Dir(target)
	dir, err := storage.OpenNoSymlinks(parent, unix.O_PATH|unix.O_DIRECTORY)
	if err != nil {
		return fmt.Errorf("open jail directory %s: %w", parent, err)
	}
	defer dir.Close()
	parentFD := int(dir.Fd())
	targetFD, err := unix.Openat(parentFD, filepath.Base(target), unix.O_CREAT|unix.O_EXCL|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("create jail mountpoint %s: %w", target, err)
	}
	defer unix.Close(targetFD)
	targetRef := fmt.Sprintf("/proc/self/fd/%d", targetFD)
	if err := unix.Mount(fmt.Sprintf("/proc/self/fd/%d", fd), targetRef, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind %s into jail: %w", source, err)
	}
	if readonly {
		// Resolve through the pinned parent after mounting: targetFD still refers
		// to the covered placeholder, not the new mount.
		mounted := fmt.Sprintf("/proc/self/fd/%d/%s", parentFD, filepath.Base(target))
		if err := unix.Mount("", mounted, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV, ""); err != nil {
			return fmt.Errorf("make jailed artifact readonly: %w", err)
		}
	}
	return nil
}

// Unmount deepest paths first and never use lazy detach: successful cleanup
// must mean the jail no longer retains any reference to a guest disk.
func unmountJail(workspace string) error {
	if !filepath.IsAbs(workspace) || filepath.Clean(workspace) != workspace || workspace == "/" {
		return fmt.Errorf("invalid jail workspace %q", workspace)
	}
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	var mounts []string
	decode := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		path := decode.Replace(fields[4])
		if path == workspace || strings.HasPrefix(path, workspace+"/") {
			mounts = append(mounts, path)
		}
	}
	sort.Slice(mounts, func(i, j int) bool { return len(mounts[i]) > len(mounts[j]) })
	for _, path := range mounts {
		if err := unix.Unmount(path, 0); err != nil {
			return fmt.Errorf("unmount jail path %s: %w", path, err)
		}
	}
	return nil
}
