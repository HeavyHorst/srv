package front

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	gssh "github.com/gliderlabs/ssh"
	"golang.org/x/crypto/ssh"
	"tailscale.com/tsnet"

	"srv/internal/model"
)

func (c Config) Validate() error {
	if c.Hostname == "" || c.DataDir == "" || c.ListenAddr == "" || len(c.AllowedUsers) == 0 || len(c.Hosts) == 0 {
		return fmt.Errorf("hostname, data_dir, listen_addr, nonempty allowed_users and hosts required")
	}
	for _, u := range c.AllowedUsers {
		if strings.TrimSpace(u) == "" || u == "*" {
			return fmt.Errorf("allowed_users must contain explicit login names")
		}
	}
	seen := map[string]bool{}
	valid := regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
	for _, h := range c.Hosts {
		if !valid.MatchString(h.Name) || seen[h.Name] {
			return fmt.Errorf("invalid or duplicate host name %q", h.Name)
		}
		seen[h.Name] = true
		if _, _, err := net.SplitHostPort(h.Address); err != nil {
			return fmt.Errorf("host %s address: %w", h.Name, err)
		}
		_, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(h.HostKey))
		if err != nil || len(bytes.TrimSpace(rest)) != 0 {
			return fmt.Errorf("host %s requires one pinned OpenSSH public host key", h.Name)
		}
	}
	return nil
}

type SSHTransport struct {
	Dial func(context.Context, string, string) (net.Conn, error)
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
func (t SSHTransport) Run(ctx context.Context, h Host, a model.Actor, args []string, in io.Reader, out, stderr io.Writer) error {
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(h.HostKey))
	if err != nil {
		return err
	}
	// Bound connection establishment, not the command or archive stream.
	connectCtx, cancelConnect := context.WithTimeout(ctx, 15*time.Second)
	defer cancelConnect()
	raw, err := t.Dial(connectCtx, "tcp", h.Address)
	if err != nil {
		return err
	}
	defer raw.Close()
	deadline, _ := connectCtx.Deadline()
	if err := raw.SetDeadline(deadline); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { raw.Close() })
	defer stop()
	conn, chans, reqs, err := ssh.NewClientConn(raw, h.Address, &ssh.ClientConfig{User: "srv-front", HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
		if !bytes.Equal(k.Marshal(), key.Marshal()) {
			return fmt.Errorf("host key pin mismatch for %s", h.Name)
		}
		return nil
	}})
	if err != nil {
		return err
	}
	if err := raw.SetDeadline(time.Time{}); err != nil {
		conn.Close()
		return err
	}
	cancelConnect()
	client := ssh.NewClient(conn, chans, reqs)
	defer client.Close()
	// Export may be silent during compression or downstream backpressure. Keep
	// the backend's SSH idle timer alive without imposing an archive duration.
	keepaliveDone := make(chan struct{})
	defer close(keepaliveDone)
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-keepaliveDone:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, _, err := client.SendRequest("keepalive@openssh.com", false, nil); err != nil {
					client.Close()
					return
				}
			}
		}
	}()
	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()
	payload, err := json.Marshal(a)
	if err != nil {
		return err
	}
	argv := append([]string{"forward", base64.RawURLEncoding.EncodeToString(payload)}, args...)
	for i := range argv {
		argv[i] = quote(argv[i])
	}
	// Do not give ssh.Session an arbitrary Reader: its internal copier can hang
	// Wait forever on idle inbound stdin. Own that copier and close stdin at EOF.
	stdin, err := session.StdinPipe()
	if err != nil {
		return err
	}
	session.Stdout = out
	session.Stderr = stderr
	if err = session.Start(strings.Join(argv, " ")); err != nil {
		return err
	}
	if in == nil {
		stdin.Close()
	} else {
		go func() { io.Copy(stdin, in); stdin.Close() }()
	}
	return session.Wait()
}
func signer(dir string) (ssh.Signer, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "ssh_host_ed25519_key")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		_, key, e := ed25519.GenerateKey(rand.Reader)
		if e != nil {
			return nil, e
		}
		block, e := ssh.MarshalPrivateKey(key, "")
		if e != nil {
			return nil, e
		}
		data = pem.EncodeToMemory(block)
		f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return nil, e
		}
		_, e = f.Write(data)
		closeErr := f.Close()
		if e != nil {
			return nil, e
		}
		if closeErr != nil {
			return nil, closeErr
		}
	} else if err != nil {
		return nil, err
	}
	return ssh.ParsePrivateKey(data)
}
func Serve(ctx context.Context, c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	key, err := signer(c.DataDir)
	if err != nil {
		return err
	}
	var tags []string
	for _, tag := range strings.Split(os.Getenv("SRV_ADVERTISE_TAGS"), ",") {
		if tag = strings.TrimSpace(tag); tag != "" {
			tags = append(tags, tag)
		}
	}
	ts := &tsnet.Server{Dir: filepath.Join(c.DataDir, "tsnet"), Hostname: c.Hostname, AuthKey: os.Getenv("TS_AUTHKEY"), ClientID: os.Getenv("TS_CLIENT_ID"), ClientSecret: os.Getenv("TS_CLIENT_SECRET"), ControlURL: os.Getenv("TS_CONTROL_URL"), AdvertiseTags: tags}
	defer ts.Close()
	status, err := ts.Up(ctx)
	if err != nil {
		return err
	}
	if status.Self != nil {
		fmt.Fprintf(os.Stderr, "srv-front stable node ID: %s (configure backend SRV_TRUSTED_FRONTENDS)\n", status.Self.ID)
	}
	local, err := ts.LocalClient()
	if err != nil {
		return err
	}
	listener, err := ts.Listen("tcp", c.ListenAddr)
	if err != nil {
		return err
	}
	defer listener.Close()
	router := &Router{Hosts: c.Hosts, Transport: SSHTransport{Dial: ts.Dial}}
	server := &gssh.Server{HostSigners: []gssh.Signer{key}, IdleTimeout: 5 * time.Minute, PtyCallback: func(gssh.Context, gssh.Pty) bool { return false }, SessionRequestCallback: func(_ gssh.Session, requestType string) bool {
		return requestType != "shell" && requestType != "subsystem"
	}, Handler: func(s gssh.Session) {
		who, err := local.WhoIs(s.Context(), s.RemoteAddr().String())
		if err != nil || who == nil || who.Node == nil || who.UserProfile == nil || len(who.Node.Tags) > 0 || who.UserProfile.LoginName == "" {
			fmt.Fprintln(s.Stderr(), "user identity required; tagged nodes rejected")
			s.Exit(1)
			return
		}
		allowed := false
		for _, u := range c.AllowedUsers {
			if strings.EqualFold(u, who.UserProfile.LoginName) {
				allowed = true
			}
		}
		if !allowed {
			fmt.Fprintln(s.Stderr(), "user is not allowed")
			s.Exit(1)
			return
		}
		actor := model.Actor{UserLogin: who.UserProfile.LoginName, DisplayName: who.UserProfile.DisplayName, NodeName: who.Node.Name, RemoteAddr: s.RemoteAddr().String(), SSHUser: s.User()}
		ctx, cancel := context.WithCancel(s.Context())
		defer cancel()
		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if _, err := s.SendRequest("keepalive@openssh.com", false, nil); err != nil {
						cancel()
						return
					}
				}
			}
		}()
		if err := router.Run(ctx, actor, s.Command(), s, s, s.Stderr()); err != nil {
			fmt.Fprintln(s.Stderr(), err)
			s.Exit(1)
			return
		}
		s.Exit(0)
	}}
	stop := context.AfterFunc(ctx, func() { server.Close() })
	defer stop()
	err = server.Serve(listener)
	if ctx.Err() != nil {
		return nil
	}
	return err
}
