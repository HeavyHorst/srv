package front_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	gssh "github.com/gliderlabs/ssh"
	"golang.org/x/crypto/ssh"
	"srv/internal/front"
	"srv/internal/model"
)

func testSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func TestSSHTransport(t *testing.T) {
	signer := testSigner(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	type request struct {
		args  []string
		actor model.Actor
		err   error
	}
	requests := make(chan request, 8)
	waiting := make(chan struct{}, 1)
	server := &gssh.Server{HostSigners: []gssh.Signer{signer}, Handler: func(s gssh.Session) {
		args := s.Command()
		var actor model.Actor
		if len(args) < 3 || args[0] != "forward" {
			requests <- request{err: errors.New("missing wrapper")}
			s.Exit(2)
			return
		}
		data, e := base64.RawURLEncoding.DecodeString(args[1])
		if e == nil {
			e = json.Unmarshal(data, &actor)
		}
		requests <- request{args: args[2:], actor: actor, err: e}
		switch args[2] {
		case "echo":
			if _, e = io.Copy(s, s); e != nil {
				s.Exit(3)
				return
			}
			s.Exit(0)
		case "fail":
			io.WriteString(s.Stderr(), "backend rejected\n")
			s.Exit(23)
		case "wait":
			waiting <- struct{}{}
			<-s.Context().Done()
		default:
			s.Exit(2)
		}
	}}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	t.Cleanup(func() { server.Close(); listener.Close(); <-served })
	dialer := &net.Dialer{}
	transport := front.SSHTransport{Dial: dialer.DialContext}
	host := front.Host{Name: "backend", Address: listener.Addr().String(), HostKey: string(ssh.MarshalAuthorizedKey(signer.PublicKey()))}
	actor := model.Actor{UserLogin: "user@example.com", DisplayName: "User Name", NodeName: "laptop", RemoteAddr: "100.1.2.3:456", SSHUser: "root"}
	t.Run("quoted arguments actor and binary stream", func(t *testing.T) {
		payload := bytes.Repeat([]byte{0, 1, 2, 255, 128, '\n', '\'', '"'}, 256*1024)
		args := []string{"echo", "two words", "single'quote", `double"quote`, "$(not-a-shell); x"}
		var out, stderr bytes.Buffer
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := transport.Run(ctx, host, actor, args, bytes.NewReader(payload), &out, &stderr); err != nil {
			t.Fatal(err)
		}
		req := <-requests
		if req.err != nil || !reflect.DeepEqual(req.args, args) || req.actor != actor {
			t.Fatalf("request=%+v", req)
		}
		if !bytes.Equal(out.Bytes(), payload) || stderr.Len() != 0 {
			t.Fatalf("stream mismatch: got %d bytes stderr=%s", out.Len(), stderr.String())
		}
	})
	t.Run("remote exit", func(t *testing.T) {
		var stderr bytes.Buffer
		err := transport.Run(context.Background(), host, actor, []string{"fail"}, nil, io.Discard, &stderr)
		<-requests
		var exit *ssh.ExitError
		if !errors.As(err, &exit) || exit.ExitStatus() != 23 || !strings.Contains(stderr.String(), "backend rejected") {
			t.Fatalf("err=%v stderr=%s", err, stderr.String())
		}
	})
	t.Run("wrong pin", func(t *testing.T) {
		wrong := host
		wrong.HostKey = string(ssh.MarshalAuthorizedKey(testSigner(t).PublicKey()))
		err := transport.Run(context.Background(), wrong, actor, []string{"echo"}, nil, io.Discard, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "pin mismatch") {
			t.Fatal(err)
		}
	})
	t.Run("cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- transport.Run(ctx, host, actor, []string{"wait"}, nil, io.Discard, io.Discard) }()
		select {
		case <-waiting:
		case <-time.After(5 * time.Second):
			t.Fatal("session did not start")
		}
		<-requests
		cancel()
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("cancelled session succeeded")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("cancellation did not interrupt transport")
		}
	})
}
