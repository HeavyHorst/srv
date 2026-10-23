package vmrunner

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Hard-link the socket as we do other jail resources: clients need not traverse
// the jailer's private directories. Its owner/group follow the trusted instance
// directory created by the control service, never request-supplied identities.
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
	return linkFileIntoJail(socket, filepath.Join(instanceDir, "orb.vsock"))
}
