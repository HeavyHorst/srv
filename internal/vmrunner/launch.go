package vmrunner

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"time"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
)

const launchInitializationTimeout = 30 * time.Second

// Retain the SDK's configuration handlers, but not its process cleanup or
// numeric-PID signal forwarding. In particular a later configuration error
// must not unlink a socket that still belongs to a surviving process.
func preserveRuntimeLaunchHandler(cmd *exec.Cmd) firecracker.Handler {
	return firecracker.Handler{
		Name: firecracker.StartVMMHandlerName,
		Fn: func(ctx context.Context, m *firecracker.Machine) error {
			if err := cmd.Start(); err != nil {
				return err
			}
			exited := make(chan struct{})
			go func() { _ = cmd.Wait(); close(exited) }()
			deadline := time.NewTimer(3 * time.Second)
			defer deadline.Stop()
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-exited:
					return fmt.Errorf("VMM exited before API socket became ready")
				case <-deadline.C:
					return fmt.Errorf("VMM API socket readiness timed out")
				case <-ticker.C:
					conn, err := net.DialTimeout("unix", m.Cfg.SocketPath, 50*time.Millisecond)
					if err == nil {
						_ = conn.Close()
						return nil
					}
				}
			}
		},
	}
}
