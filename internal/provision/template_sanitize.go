package provision

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Never mount guest filesystems on the host. Paths are quoted for debugfs,
// unsupported control/escape characters and intermediate symlinks are rejected.
// debugfs returns success for many errors: stderr AND parsed results are checked.

type templateDisk struct {
	ctx       context.Context
	path      string
	remaining int
}
type templateEntry struct {
	name string
	mode uint64
}

func (d *templateDisk) command(operation, path string) (string, error) {
	if strings.ContainsAny(path, "\x00\r\n\"\\") {
		return "", errors.New("unsupported escape or control character in cleanup path")
	}
	command := operation + " \"" + path + "\""
	cmd := exec.CommandContext(d.ctx, "debugfs", "-w", "-R", command, d.path)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	if len(lines) > 0 && strings.HasPrefix(lines[0], "debugfs ") {
		lines = lines[1:]
	}
	if err != nil || strings.TrimSpace(strings.Join(lines, "\n")) != "" {
		return "", fmt.Errorf("debugfs %s failed: %v: %s", command, err, strings.Join(lines, "\n"))
	}
	return string(out), nil
}

func (d *templateDisk) list(path string) ([]templateEntry, error) {
	out, err := d.command("ls -p", path)
	if err != nil {
		return nil, err
	}
	var entries []templateEntry
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Split(line, "/")
		if len(f) != 8 {
			return nil, errors.New("unexpected debugfs directory output")
		}
		if f[1] == "0" {
			continue
		}
		mode, err := strconv.ParseUint(f[2], 8, 32)
		if err != nil {
			return nil, errors.New("unsupported guest directory entry")
		}
		if f[5] != "." && f[5] != ".." {
			entries = append(entries, templateEntry{f[5], mode})
		}
	}
	return entries, nil
}

func (d *templateDisk) remove(path string, entry templateEntry, depth int) error {
	d.remaining--
	if d.remaining < 0 || depth > 64 {
		return errors.New("template cleanup exceeds traversal limit")
	}
	command := "rm"
	if entry.mode&0170000 == 0040000 {
		entries, err := d.list(path)
		if err != nil {
			return err
		}
		for _, child := range entries {
			if err := d.remove(path+"/"+child.name, child, depth+1); err != nil {
				return err
			}
		}
		command = "rmdir"
	}
	out, err := d.command(command, path)
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) != "" {
		return errors.New("unexpected debugfs removal output")
	}
	return nil
}

func (d *templateDisk) clean(path string) error {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	parent := ""
	for i, part := range parts {
		entries, err := d.list(parent + "/")
		if err != nil {
			return err
		}
		var found *templateEntry
		for _, entry := range entries {
			if entry.name == part {
				e := entry
				found = &e
				break
			}
		}
		if found == nil {
			return nil
		}
		parent += "/" + part
		if i != len(parts)-1 {
			if found.mode&0170000 != 0040000 {
				return fmt.Errorf("unsafe non-directory cleanup ancestor %s", parent)
			}
			continue
		}
		if err := d.remove(parent, *found, 0); err != nil {
			return err
		}
		// Verify deletion independently rather than trusting debugfs's exit status.
		verifyPath := strings.TrimSuffix(parent, "/"+part)
		remaining, err := d.list(verifyPath + "/")
		if err != nil {
			return err
		}
		for _, entry := range remaining {
			if entry.name == part {
				return errors.New("guest identity cleanup verification failed")
			}
		}
	}
	return nil
}

func sanitizeTemplateDisk(ctx context.Context, path string) error {
	if err := runRootFSCheckContext(ctx, path, "check template filesystem before cleanup"); err != nil {
		return err
	}
	d := templateDisk{ctx: ctx, path: path, remaining: 100000}
	for _, target := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id", "/var/lib/tailscale", "/var/lib/srv/bootstrap.done", "/var/lib/systemd/random-seed"} {
		if err := d.clean(target); err != nil {
			return err
		}
	}
	// Preserve prepared settings, skills, installed tools and setup state. Only
	// remove named authentication/session files, never whole Amp directories.
	// Bootstrap receives fresh ORB_TOKEN in /run; these disks remain confidential.
	for _, home := range []string{"/root", "/home/user"} {
		for _, suffix := range []string{"/.config/amp/secrets.json", "/.local/share/amp/secrets.json", "/.local/share/amp/session.json", "/.local/share/amp/device-id.json"} {
			if err := d.clean(home + suffix); err != nil {
				return err
			}
		}
	}
	return runRootFSCheckContext(ctx, path, "check template filesystem after cleanup")
}
