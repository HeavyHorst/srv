package storage

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The grandchild remains alive after SIGKILL of its immediate parent. All three
// independent locks must remain held until the grandchild closes its descriptors.
func TestInheritedLocksSurviveParentSIGKILL(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"payload", "namespace", "name"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestPayloadLockProcess$")
	cmd.Env = append(os.Environ(), "SRV_LOCK_PROCESS=parent", "SRV_LOCK_DIR="+dir)
	cmd.Stdin = r
	cmd.Stderr = os.Stderr
	ready, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	got := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(ready).ReadString('\n'); got <- line }()
	select {
	case line := <-got:
		if line != "ready\n" {
			t.Fatalf("child did not become ready: %q", line)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("child startup timed out")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("parent unexpectedly survived SIGKILL")
	}
	for _, name := range []string{"payload", "namespace", "name"} {
		if err := tryPayloadTestLock(filepath.Join(dir, name)); !errors.Is(err, unix.EWOULDBLOCK) {
			t.Fatalf("%s lock lost when parent died: %v", name, err)
		}
	}
	_ = w.Close() // EOF lets the surviving child close all inherited fds.
	deadline := time.Now().Add(10 * time.Second)
	for _, name := range []string{"payload", "namespace", "name"} {
		for {
			err := tryPayloadTestLock(filepath.Join(dir, name))
			if err == nil {
				break
			}
			if !errors.Is(err, unix.EWOULDBLOCK) || time.Now().After(deadline) {
				t.Fatalf("%s lock not released on child exit: %v", name, err)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func TestPayloadLockProcess(t *testing.T) {
	switch os.Getenv("SRV_LOCK_PROCESS") {
	case "parent":
		ctx := context.Background()
		for _, name := range []string{"payload", "namespace", "name"} {
			f, err := os.OpenFile(filepath.Join(os.Getenv("SRV_LOCK_DIR"), name), os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
				t.Fatal(err)
			}
			ctx = WithInheritedLock(ctx, f)
		}
		cmd := exec.Command(os.Args[0], "-test.run=^TestPayloadLockProcess$")
		cmd.Env = append(os.Environ(), "SRV_LOCK_PROCESS=child")
		// Optional real CLI check on a disposable Btrfs host. Its stdin pipe
		// remains open in the test process after this parent is killed.
		if dest := os.Getenv("SRV_LOCK_BTRFS_RECEIVE"); dest != "" {
			cmd = exec.Command("btrfs", "receive", dest)
		}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		InheritLocks(ctx, cmd)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		if os.Getenv("SRV_LOCK_BTRFS_RECEIVE") != "" {
			// Wait for exec and descriptor handling to finish, rather than
			// proving only that the pre-exec fork inherited the locks.
			time.Sleep(100 * time.Millisecond)
			fmt.Fprintln(os.Stdout, "ready")
		}
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
		os.Exit(0)
	case "child":
		fmt.Fprintln(os.Stdout, "ready")
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	case "inspect":
		if err := unix.Flock(3, unix.LOCK_EX|unix.LOCK_NB); err != nil {
			t.Fatalf("storage command did not inherit held lock: %v", err)
		}
		os.Exit(0)
	}
}

func TestStorageCommandInheritsLocks(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink(os.Args[0], filepath.Join(dir, "btrfs")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("SRV_LOCK_PROCESS", "inspect")
	f, err := os.CreateTemp(dir, "lock")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	ctx := WithInheritedLock(context.Background(), f)
	if _, err := Command(ctx, "-test.run=^TestPayloadLockProcess$"); err != nil {
		t.Fatal(err)
	}
}

func tryPayloadTestLock(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

func TestInheritedLocksAreOptInAndPreserveExtraFiles(t *testing.T) {
	a, err := os.CreateTemp(t.TempDir(), "a")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := os.CreateTemp(t.TempDir(), "b")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	base := WithInheritedLock(context.Background(), a)
	ctx := WithInheritedLock(base, b)
	ctx = WithInheritedLock(ctx, a)
	cmd := exec.Command("unused")
	cmd.ExtraFiles = []*os.File{b}
	InheritLocks(ctx, cmd)
	InheritLocks(ctx, cmd)
	if len(cmd.ExtraFiles) != 2 || cmd.ExtraFiles[0] != b || cmd.ExtraFiles[1] != a {
		t.Fatalf("existing files moved or inherited locks duplicated: %v", cmd.ExtraFiles)
	}
	plain := exec.CommandContext(ctx, "unused")
	if len(plain.ExtraFiles) != 0 {
		t.Fatal("commands inherited locks without opting in")
	}
	other := exec.Command("unused")
	InheritLocks(base, other)
	if len(other.ExtraFiles) != 1 || other.ExtraFiles[0] != a {
		t.Fatal("derived context mutated base lock set")
	}
}

func payloadRootFixture(t *testing.T) (string, string) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("requires real root-owned lock trust chain")
	}
	dir, err := os.MkdirTemp("/root", "srv-payload-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	locks := filepath.Join(dir, ".host-locks")
	if err := os.Mkdir(locks, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(locks, "payload.lock")
	if err := os.WriteFile(path, nil, 0660); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0660); err != nil {
		t.Fatal(err)
	}
	return dir, path
}

func TestPayloadSerializationAndCancellation(t *testing.T) {
	dir, path := payloadRootFixture(t)
	ctx, release, err := acquirePayload(context.Background(), dir, os.Getegid())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	cmd := exec.Command("unused")
	InheritLocks(ctx, cmd)
	if len(cmd.ExtraFiles) != 1 {
		t.Fatal("acquired payload lock missing from context")
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if _, _, err := acquirePayload(waitCtx, dir, os.Getegid()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contending payload acquire did not cancel: %v", err)
	}
	// A duplicate fd shares the open-file-description, exactly as exec does.
	fd, err := unix.Dup(int(cmd.ExtraFiles[0].Fd()))
	if err != nil {
		t.Fatal(err)
	}
	inherited := os.NewFile(uintptr(fd), "inherited-payload")
	defer inherited.Close()
	release()
	if err := tryPayloadTestLock(path); !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatalf("release explicitly unlocked inherited description: %v", err)
	}
	_ = inherited.Close()
	if err := tryPayloadTestLock(path); err != nil {
		t.Fatalf("last inherited close did not release lock: %v", err)
	}
	_, release, err = acquirePayload(context.Background(), dir, os.Getegid())
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func TestPayloadRejectsUnsafeNamespace(t *testing.T) {
	for _, mode := range []string{"missing", "symlink", "hardlink", "fifo", "directory", "file-mode", "file-owner", "file-group", "lock-directory-mode", "data-directory-mode", "data-directory-owner", "lock-directory-symlink", "ancestor-symlink"} {
		t.Run(mode, func(t *testing.T) {
			dir, path := payloadRootFixture(t)
			var err error
			switch mode {
			case "missing":
				err = os.Remove(path)
			case "symlink", "fifo", "directory":
				if err = os.Remove(path); err == nil {
					switch mode {
					case "symlink":
						err = os.Symlink("target", path)
					case "fifo":
						err = unix.Mkfifo(path, 0660)
					case "directory":
						err = os.Mkdir(path, 0660)
					}
				}
			case "hardlink":
				err = os.Link(path, path+".alias")
			case "file-mode":
				err = os.Chmod(path, 0666)
			case "file-owner":
				err = os.Chown(path, 1000, -1)
			case "file-group":
				err = os.Chown(path, -1, os.Getegid()+1)
			case "lock-directory-mode":
				err = os.Chmod(filepath.Dir(path), 0775)
			case "data-directory-mode":
				err = os.Chmod(dir, 0777)
			case "data-directory-owner":
				err = os.Chown(dir, 1000, -1)
			case "lock-directory-symlink":
				if err = os.Rename(filepath.Dir(path), filepath.Dir(path)+".real"); err == nil {
					err = os.Symlink(".host-locks.real", filepath.Dir(path))
				}
			case "ancestor-symlink":
				alias := dir + ".alias"
				err = os.Symlink(dir, alias)
				t.Cleanup(func() { _ = os.Remove(alias) })
				dir = alias
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, release, err := acquirePayload(context.Background(), dir, os.Getegid()); err == nil {
				release()
				t.Fatalf("accepted unsafe %s", mode)
			}
			if mode == "missing" {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("runtime created missing payload lock: %v", err)
				}
			}
		})
	}
}

func TestPayloadRejectsUnprivilegedRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires an unprivileged owner")
	}
	if _, release, err := acquirePayload(context.Background(), t.TempDir(), os.Getegid()); err == nil {
		release()
		t.Fatal("accepted unprivileged or writable-ancestor data root")
	}
}
