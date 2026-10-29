package vmrunner

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Hard-link the socket so clients need not traverse the jailer's private
// directories. Runtime directories (unlike disks) share the data subvolume.
// Its owner/group follow the trusted instance directory, never request identities.
func exposeOrbSocket(instanceDir, jailRoot string) error {
	info, err := os.Stat(instanceDir)
	if err != nil {
		return err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cannot determine instance directory owner")
	}
	socket := filepath.Join(jailRoot, "orb.vsock")
	info, err = os.Lstat(socket)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("orb transport is not a socket")
	}
	if err := os.Chown(socket, int(owner.Uid), int(owner.Gid)); err != nil {
		return err
	}
	if err := os.Chmod(socket, 0o660); err != nil {
		return err
	}
	target := filepath.Join(instanceDir, "orb.vsock")
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Link(socket, target); err != nil {
		return fmt.Errorf("expose orb socket (jailer and instance runtime directories must share a subvolume): %w", err)
	}
	return nil
}
