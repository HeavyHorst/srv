package coldstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"srv/internal/storage"
)

// DefaultTimeout accommodates large disk transfers; both peers must allow it.
const DefaultTimeout = 6 * time.Hour
const defaultTimeout = DefaultTimeout
const cleanupTimeout = 30 * time.Second

type Client struct{ Config Config }

var hostPattern = regexp.MustCompile(`^(root@)?[a-zA-Z0-9][a-zA-Z0-9.:-]*$`)

// Check the whole trust chain: a safe leaf can still be replaced through a
// writable or user-owned ancestor. No sticky-directory exception is permitted.
func validateCredential(path string, lstat func(string) (os.FileInfo, error), private ...bool) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("SSH credential path must be absolute and clean")
	}
	for p := path; ; p = filepath.Dir(p) {
		st, err := lstat(p)
		if err != nil {
			return err
		}
		raw, ok := st.Sys().(*syscall.Stat_t)
		if !ok || raw.Uid != 0 || st.Mode().Perm()&0022 != 0 {
			return fmt.Errorf("SSH credential component %s must be root-owned and not writable by group or others", p)
		}
		if p == path {
			if len(private) > 0 && private[0] && st.Mode().Perm()&0077 != 0 {
				return errors.New("private SSH key must not grant group or other permissions")
			}
			if !st.Mode().IsRegular() {
				return errors.New("SSH credentials must be regular files")
			}
		} else if !st.IsDir() {
			return errors.New("SSH credential ancestors must be nonsymlink directories")
		}
		if p == "/" {
			return nil
		}
	}
}

// Validate allows an entirely disabled configuration. Zero Timeout selects the
// default; a negative timeout is never meaningful for an enabled transport.
func (c Config) Validate() error {
	if c.Host == "" {
		return nil
	}
	if c.Timeout < 0 {
		return errors.New("cold store timeout must be nonnegative")
	}
	if err := c.validateSSH(); err != nil {
		return err
	}
	for _, name := range []string{"ssh", "btrfs"} {
		if _, err := exec.LookPath(name); err != nil {
			return fmt.Errorf("cold store requires %s: %w", name, err)
		}
	}
	return nil
}

func (c Config) validateSSH() error {
	if !hostPattern.MatchString(c.Host) {
		return errors.New("host must be a hostname/IP, optionally prefixed root@")
	}
	for i, p := range []string{c.IdentityFile, c.KnownHostsFile} {
		if err := validateCredential(p, os.Lstat, i == 0); err != nil {
			return err
		}
		// OpenSSH expands tokens in filenames. Do not allow administrator paths
		// whose actual interpretation differs from the validated filesystem path.
		for _, ch := range p {
			if ch <= ' ' || ch == '%' || ch == '"' || ch == '\'' || ch == '\\' {
				return errors.New("unsafe SSH filename")
			}
		}
	}
	return nil
}

func (c Client) sshArgs(r request) ([]string, error) {
	if err := c.Config.validateSSH(); err != nil {
		return nil, err
	}
	command, err := encodeRequest(r)
	if err != nil {
		return nil, err
	}
	return []string{"-F", "/dev/null", "-T", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none", "-o", "GlobalKnownHostsFile=/dev/null", "-o", "UserKnownHostsFile=" + c.Config.KnownHostsFile, "-o", "ConnectTimeout=15", "-o", "ConnectionAttempts=1", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=2", "-o", "ClearAllForwardings=yes", "-o", "PermitLocalCommand=no", "-o", "ProxyCommand=none", "-l", "root", "-i", c.Config.IdentityFile, "--", c.Config.Host, command}, nil
}

func bounded(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

// limitBuffer keeps subprocess diagnostics and metadata bounded without making
// the child block when it writes more than the retained diagnostic prefix.
type limitBuffer struct {
	buffer   bytes.Buffer
	overflow bool
}

func (b *limitBuffer) Len() int                   { return b.buffer.Len() }
func (b *limitBuffer) String() string             { return b.buffer.String() }
func (b *limitBuffer) Read(p []byte) (int, error) { return b.buffer.Read(p) }

func (b *limitBuffer) Write(p []byte) (int, error) {
	n := len(p)
	keep := maxMetadata - b.Len()
	if len(p) > keep {
		p = p[:keep]
		b.overflow = true
	}
	_, _ = b.buffer.Write(p)
	return n, nil
}

func run(ctx context.Context, name string, args []string, in io.Reader, out io.Writer) error {
	cmd := exec.CommandContext(ctx, name, args...)
	// Pin local storage writers, not the transport: OpenSSH closes inherited
	// descriptors. Remote mutations have their own namespace lock and journal.
	if name == "btrfs" {
		storage.InheritLocks(ctx, cmd)
	}
	cmd.Stdin, cmd.Stdout = in, out
	var stderr limitBuffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, stderr.String())
	}
	return nil
}

// pipeline waits for both processes, cancelling the sibling immediately on
// failure. OS pipes avoid uninterruptible Go copier goroutines on broken peers.
func pipeline(ctx context.Context, leftName string, leftArgs []string, rightName string, rightArgs []string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	defer r.Close()
	defer w.Close()
	left := exec.CommandContext(ctx, leftName, leftArgs...)
	right := exec.CommandContext(ctx, rightName, rightArgs...)
	if leftName == "btrfs" {
		storage.InheritLocks(ctx, left)
	}
	if rightName == "btrfs" {
		storage.InheritLocks(ctx, right)
	}
	var le, re limitBuffer
	left.Stdout, left.Stderr = w, &le
	right.Stdin, right.Stderr = r, &re
	left.WaitDelay, right.WaitDelay = time.Second, time.Second
	if err := right.Start(); err != nil {
		return err
	}
	if err := left.Start(); err != nil {
		cancel()
		_ = right.Wait()
		return err
	}
	_ = r.Close()
	_ = w.Close()
	results := make(chan error, 2)
	go func() { results <- left.Wait() }()
	go func() { results <- right.Wait() }()
	a := <-results
	if a != nil {
		cancel()
	}
	b := <-results
	if a != nil || b != nil {
		return fmt.Errorf("transfer: %w; sender: %s; receiver: %s", errors.Join(a, b), le.String(), re.String())
	}
	return nil
}

func (c Client) stat(ctx context.Context, id string) (Object, error) {
	var o Object
	args, err := c.sshArgs(request{Operation: "stat", Object: Object{ID: id}})
	if err != nil {
		return o, err
	}
	var out limitBuffer
	if err := run(ctx, "ssh", args, nil, &out); err != nil {
		return o, err
	}
	if out.overflow {
		return o, errors.New("oversized stat response")
	}
	if err := decode(&out, &o); err != nil {
		return o, err
	}
	if err := o.validate(); err != nil {
		return o, err
	}
	if o.ID != id {
		return o, errors.New("stat returned wrong object ID")
	}
	return o, nil
}

func (c Client) Stat(ctx context.Context, id string) (Object, error) {
	ctx, cancel := bounded(ctx, c.Config.Timeout)
	defer cancel()
	return c.stat(ctx, id)
}

func (c Client) Put(ctx context.Context, o Object, snapshotPath, parentPath string) error {
	ctx, cancel := bounded(ctx, c.Config.Timeout)
	defer cancel()
	if err := o.validate(); err != nil {
		return err
	}
	if (parentPath == "") != (o.Parent == "") {
		return errors.New("parent path and object parent must be supplied together")
	}
	if err := verifyDiskIdentity(ctx, snapshotPath, o, false); err != nil {
		return err
	}
	args, err := c.sshArgs(request{Operation: "put", Object: o})
	if err != nil {
		return err
	}
	// An unsuccessful stat is not proof of absence. Only a successful exact
	// match skips upload; the remote put independently refuses all overwrites.
	if existing, err := c.stat(ctx, o.ID); err == nil {
		if existing != o {
			return errors.New("immutable object already exists with different metadata")
		}
		return nil
	}
	if o.Parent != "" {
		parent, err := c.stat(ctx, o.Parent)
		if err != nil {
			return fmt.Errorf("parent: %w", err)
		}
		if err := verifyDiskIdentity(ctx, parentPath, parent, false); err != nil {
			return err
		}
	}
	if err := pipeline(ctx, "btrfs", sendArgs(snapshotPath, parentPath), "ssh", args); err != nil {
		// Do not delete an object after a lost acknowledgement: it may already
		// have been durably published. A later exact Stat/Put resolves ambiguity.
		return err
	}
	actual, err := c.stat(ctx, o.ID)
	if err != nil {
		return err
	}
	if actual != o {
		return errors.New("uploaded object metadata mismatch")
	}
	return nil
}

func (c Client) Get(ctx context.Context, o Object, destDir, parentID string) (string, error) {
	ctx, cancel := bounded(ctx, c.Config.Timeout)
	defer cancel()
	if err := o.validate(); err != nil {
		return "", err
	}
	args, err := c.sshArgs(request{Operation: "get", Object: Object{ID: o.ID}, Parent: parentID})
	if err != nil {
		return "", err
	}
	actual, err := c.stat(ctx, o.ID)
	if err != nil {
		return "", err
	}
	if actual != o {
		return "", errors.New("remote object metadata mismatch")
	}
	if err := storage.RequireBtrfs(destDir); err != nil {
		return "", err
	}
	st, err := os.Stat(destDir)
	if err != nil {
		return "", err
	}
	if !st.IsDir() || st.Mode().Perm()&0022 != 0 {
		return "", errors.New("receive destination must be a non-writable-by-others directory")
	}
	stage, err := os.MkdirTemp(destDir, ".cold-receive-")
	if err != nil {
		return "", err
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		_ = storage.RemoveTree(cleanupCtx, stage)
	}()
	if err := pipeline(ctx, "ssh", args, "btrfs", []string{"receive", stage}); err != nil {
		return "", err
	}
	disk, err := receivedDisk(ctx, stage, o)
	if err != nil {
		return "", err
	}
	if err := storage.SyncPath(filepath.Join(disk, "rootfs.img")); err != nil {
		return "", err
	}
	if err := storage.SyncPath(disk); err != nil {
		return "", err
	}
	if err := storage.SyncPath(stage); err != nil {
		return "", err
	}
	dest := filepath.Join(destDir, o.ID)
	// Publish the ordinary container: RENAME_NOREPLACE on a read-only Btrfs
	// subvolume can return EROFS. Keep the received disk immutable throughout.
	if err := unix.Renameat2(unix.AT_FDCWD, stage, unix.AT_FDCWD, dest, unix.RENAME_NOREPLACE); err != nil {
		return "", err
	}
	if err := storage.SyncPath(destDir); err != nil {
		return "", err
	}
	return filepath.Join(dest, filepath.Base(disk)), nil
}

func (c Client) Delete(ctx context.Context, id string) error {
	ctx, cancel := bounded(ctx, c.Config.Timeout)
	defer cancel()
	args, err := c.sshArgs(request{Operation: "delete", Object: Object{ID: id}})
	if err != nil {
		return err
	}
	return run(ctx, "ssh", args, nil, io.Discard)
}
