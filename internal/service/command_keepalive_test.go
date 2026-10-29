package service

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	gssh "github.com/gliderlabs/ssh"
	"golang.org/x/crypto/ssh"

	"srv/internal/model"
)

// Real TCP/SSH transport and gliderlabs IdleTimeout, not a SendRequest mock.
func TestCommandKeepAliveOverSSH(t *testing.T) {
	for _, tc := range []struct {
		command string
		cancel  bool
		wantOK  bool
	}{
		{"start vm", false, true},
		{"stop vm", false, true},
		{"restart vm", false, true},
		{"disk hydrate vm", false, true},
		{"delete vm", false, true},
		{"orb bootstrap vm", false, true},
		{"list", false, false},    // Same quiet workload without keepalive times out.
		{"start vm", true, false}, // Explicit stop restores the idle timeout.
	} {
		t.Run(tc.command+map[bool]string{true: " canceled"}[tc.cancel], func(t *testing.T) {
			_, key, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			signer, err := ssh.NewSignerFromKey(key)
			if err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			const output = "{\"phase\":\"hot\"}\n\x00\xff"
			handlerDone := make(chan struct{})
			server := &gssh.Server{
				HostSigners: []gssh.Signer{signer},
				IdleTimeout: 400 * time.Millisecond,
				Handler: func(sess gssh.Session) {
					defer close(handlerDone)
					stop := startCommandKeepAlive(sess, sess.Command(), 40*time.Millisecond)
					defer stop()
					if tc.cancel {
						stop()
					}
					select {
					case <-sess.Context().Done():
						return
					case <-time.After(1200 * time.Millisecond):
					}
					_, _ = io.WriteString(sess, output)
					_ = sess.Exit(0)
				},
			}
			defer server.Close()
			go server.Serve(listener)
			client, err := ssh.Dial("tcp", listener.Addr().String(), &ssh.ClientConfig{
				User: "test", HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 3 * time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			session, err := client.NewSession()
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			var stdout, stderr bytes.Buffer
			session.Stdout, session.Stderr = &stdout, &stderr
			result := make(chan error, 1)
			go func() { result <- session.Run(tc.command) }()
			select {
			case err := <-result:
				if (err == nil) != tc.wantOK {
					t.Fatalf("Run error = %v; want success %t", err, tc.wantOK)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("SSH command did not terminate")
			}
			wantOutput := ""
			if tc.wantOK {
				wantOutput = output
			}
			if stdout.String() != wantOutput || stderr.Len() != 0 {
				t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
			}
			select {
			case <-handlerDone:
			case <-time.After(time.Second):
				t.Fatal("handler did not stop on completion/disconnect")
			}
		})
	}
}

func TestDiskStatusDoesNotWaitForInstanceLockAndPreservesVisibility(t *testing.T) {
	st := newServiceTestStore(t)
	inst := serviceTestInstance("private", model.StateStopped, time.Now())
	inst.CreatedByUser = "owner@example.com"
	if err := st.CreateInstance(context.Background(), inst); err != nil {
		t.Fatal(err)
	}
	app := &App{store: st}
	unlock := app.lockInstance(inst.Name)
	defer unlock()
	done := make(chan commandResult, 1)
	go func() {
		result, _ := app.cmdDisk(context.Background(), model.Actor{UserLogin: "other@example.com"}, []string{"disk", "status", inst.Name}, outputFormatJSON)
		done <- result
	}()
	select {
	case result := <-done:
		if result.exitCode != 1 || !strings.Contains(result.stderr, "does not exist") {
			t.Fatalf("status leaked private instance: %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("status waited for a transfer's instance lock")
	}
}
