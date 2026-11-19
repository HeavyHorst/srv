// Command srv-wazuh-enroll registers an srv guest with the Wazuh manager and
// delivers its agent identity, or retires a decommissioned asset's agent.
//
//	srv-wazuh-enroll enroll -asset ID -incarnation ID -seq N -name HOST \
//		-guest HOST:22 -host-key SHA256:... [-ssh-key FILE]
//	srv-wazuh-enroll retire -asset ID -incarnation ID -seq N
//
// Configuration and the API credential come from the environment; see
// wazuh.ConfigFromEnv.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/crypto/ssh"

	"srv/internal/wazuh"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "srv-wazuh-enroll:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 || (args[0] != "enroll" && args[0] != "retire") {
		return fmt.Errorf("usage: srv-wazuh-enroll enroll|retire [flags]")
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	asset := fs.String("asset", "", "stable asset ID (NRC asset)")
	incID := fs.String("incarnation", "", "srv instance ID")
	seq := fs.Int64("seq", 0, "incarnation sequence, increasing with every clean rebuild")
	name := fs.String("name", "", "agent name (the guest hostname)")
	guestAddr := fs.String("guest", "", "guest SSH address host:port")
	hostKey := fs.String("host-key", "", "pinned guest SSH host key fingerprint SHA256:...")
	sshKey := fs.String("ssh-key", "", "private key file; omit for Tailscale SSH")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *asset == "" || *incID == "" || *seq <= 0 {
		return fmt.Errorf("-asset, -incarnation and a positive -seq are required")
	}
	cfg, err := wazuh.ConfigFromEnv(os.Getenv)
	if err != nil {
		return err
	}
	c, err := cfg.Coordinator()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	inc := wazuh.Incarnation{ID: *incID, Seq: *seq}
	if args[0] == "retire" {
		return c.Retire(ctx, *asset, inc)
	}
	if *name == "" || *guestAddr == "" {
		return fmt.Errorf("-name and -guest are required")
	}
	var signer ssh.Signer
	if *sshKey != "" {
		pem, err := os.ReadFile(*sshKey)
		if err != nil {
			return err
		}
		if signer, err = ssh.ParsePrivateKey(pem); err != nil {
			return err
		}
	}
	client, err := wazuh.DialGuest(ctx, nil, *guestAddr, *hostKey, signer)
	if err != nil {
		return fmt.Errorf("connecting to guest: %w", err)
	}
	defer client.Close()
	id, err := c.Enroll(ctx, *asset, inc, *name, wazuh.CommandGuest{Run: wazuh.SSHRunner(client)})
	if err != nil {
		return err
	}
	fmt.Printf("agent %s %s is reporting\n", id, *name)
	return nil
}
