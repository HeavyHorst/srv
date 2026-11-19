package wazuh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// Guest-side paths, matching images/arch-base/overlay.
const (
	writeEnrollmentCommand = "umask 077 && install -d -m 0700 /run/srv-wazuh && " +
		"cat >/run/srv-wazuh/.enrollment.json.tmp && " +
		"mv -f /run/srv-wazuh/.enrollment.json.tmp /run/srv-wazuh/enrollment.json"
	startEnrollmentCommand = "systemctl start srv-wazuh-enroll.service"
	// probeAgentCommand succeeds only when the agent package and srv's
	// enrollment units are installed (as by the image or an in-place install).
	probeAgentCommand = "test -x /var/ossec/bin/wazuh-agentd && test -x /usr/local/lib/srv/wazuh-enroll.sh && " +
		"systemctl cat srv-wazuh-enroll.service wazuh-agent.service >/dev/null"
)

// Runner runs one command as root on the guest with stdin.
type Runner func(ctx context.Context, command string, stdin []byte) error

// CommandGuest delivers the identity through a Runner. The file is written
// by root under umask 077, so it is root-owned 0600 as srv-wazuh-enroll
// requires, and renamed into place so the unit never reads a partial file.
type CommandGuest struct{ Run Runner }

func (g CommandGuest) WriteEnrollment(ctx context.Context, identity []byte) error {
	return g.Run(ctx, writeEnrollmentCommand, identity)
}

func (g CommandGuest) StartEnrollment(ctx context.Context) error {
	return g.Run(ctx, startEnrollmentCommand, nil)
}

func (g CommandGuest) ProbeAgent(ctx context.Context) error {
	return g.Run(ctx, probeAgentCommand, nil)
}

// SSHRunner runs commands over an SSH client the caller has authenticated
// as root, with the guest's host key pinned.
func SSHRunner(client *ssh.Client) Runner {
	return func(ctx context.Context, command string, stdin []byte) error {
		session, err := client.NewSession()
		if err != nil {
			return err
		}
		defer session.Close()
		session.Stdin = bytes.NewReader(stdin)
		type result struct {
			output []byte
			err    error
		}
		done := make(chan result, 1)
		go func() {
			output, err := session.CombinedOutput(command)
			done <- result{output, err}
		}()
		select {
		case r := <-done:
			if r.err != nil {
				return fmt.Errorf("%w: %s", r.err, bytes.TrimSpace(r.output))
			}
			return nil
		case <-ctx.Done():
			session.Close()
			return ctx.Err()
		}
	}
}

// Dialer opens a network connection, such as net.Dialer.DialContext or a
// tsnet server's Dial.
type Dialer func(ctx context.Context, network, addr string) (net.Conn, error)

// DialGuest opens an SSH connection to a guest as root, accepting only the
// ed25519 host key with the given SHA256 fingerprint (as reported by srv-boot,
// advertised by Tailscale SSH, or shown by `ssh-keygen -lf`). dial nil means a
// plain TCP dial. signer may be nil for Tailscale SSH, which authorises the
// tailnet identity instead of a key.
func DialGuest(ctx context.Context, dial Dialer, addr, fingerprint string, signer ssh.Signer) (*ssh.Client, error) {
	if !strings.HasPrefix(fingerprint, "SHA256:") {
		return nil, fmt.Errorf("host key fingerprint %q is not SHA256:...", fingerprint)
	}
	config := &ssh.ClientConfig{
		User: "root",
		// The pin is an ed25519 key; never let the server pick another type.
		HostKeyAlgorithms: []string{ssh.KeyAlgoED25519},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			if got := ssh.FingerprintSHA256(key); got != fingerprint {
				return fmt.Errorf("guest host key %s is not the pinned %s", got, fingerprint)
			}
			return nil
		},
		Timeout: 30 * time.Second,
	}
	if signer != nil {
		config.Auth = []ssh.AuthMethod{ssh.PublicKeys(signer)}
	}
	if dial == nil {
		dial = (&net.Dialer{Timeout: 30 * time.Second}).DialContext
	}
	conn, err := dial(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	// Bound the handshake by ctx as well as the timeout.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	c, chans, reqs, err := ssh.NewClientConn(conn, addr, config)
	if !stop() {
		err = errors.Join(err, ctx.Err())
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	return ssh.NewClient(c, chans, reqs), nil
}

// HostKeyFingerprint returns the SHA256 fingerprint of the one ed25519 key
// among authorized_keys-format host keys, such as a Tailscale peer's
// SSH_HostKeys.
func HostKeyFingerprint(keys []string) (string, error) {
	var found string
	for _, line := range keys {
		key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
		if err != nil || key.Type() != ssh.KeyAlgoED25519 {
			continue
		}
		fp := ssh.FingerprintSHA256(key)
		if found != "" && found != fp {
			return "", errors.New("guest advertises more than one ed25519 SSH host key")
		}
		found = fp
	}
	if found == "" {
		return "", errors.New("guest advertises no ed25519 SSH host key")
	}
	return found, nil
}
