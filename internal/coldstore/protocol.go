// Package coldstore implements a privileged btrfs transport to an explicitly
// trusted, administrator-configured SSH server. btrfs receive is not a security
// boundary against malicious streams. Never expose this package to arbitrary
// servers or grant the srv daemon capabilities to use it.
package coldstore

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
)

const protocol = "srv-cold-store-v1"
const maxMetadata = 4096

type Config struct {
	Host           string
	IdentityFile   string
	KnownHostsFile string
	Timeout        time.Duration
}

// UUID is the original sending subvolume UUID; ID is the immutable object key.
// Parent records a remote object dependency, not a local pathname.
type Object struct {
	ID     string `json:"id"`
	UUID   string `json:"uuid"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Parent string `json:"parent,omitempty"`
}

type request struct {
	Operation string `json:"operation"`
	Object    Object `json:"object"`
	Parent    string `json:"parent,omitempty"`
}

func validID(s string) bool {
	u, err := uuid.Parse(s)
	return err == nil && u != uuid.Nil && u.String() == s
}

func (o Object) validate() error {
	if !validID(o.ID) || !validID(o.UUID) || o.Size <= 0 || (o.Parent != "" && (!validID(o.Parent) || o.Parent == o.ID)) {
		return errors.New("invalid cold object identity, parent, or size")
	}
	b, err := hex.DecodeString(o.SHA256)
	if err != nil || len(b) != 32 || strings.ToLower(o.SHA256) != o.SHA256 {
		return errors.New("invalid SHA256")
	}
	return nil
}

func decode(r io.Reader, dst any) error {
	b, err := io.ReadAll(io.LimitReader(r, maxMetadata+1))
	if err != nil {
		return err
	}
	if len(b) > maxMetadata {
		return errors.New("metadata too large")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("trailing metadata")
	}
	return nil
}

func (r request) validate() error {
	if !validID(r.Object.ID) {
		return errors.New("invalid object ID")
	}
	switch r.Operation {
	case "put":
		if r.Parent != "" {
			return errors.New("unexpected parent")
		}
		return r.Object.validate()
	case "get":
		if r.Parent != "" && (!validID(r.Parent) || r.Parent == r.Object.ID) {
			return errors.New("invalid get parent")
		}
	case "stat", "delete":
		if r.Parent != "" {
			return errors.New("unexpected parent")
		}
	default:
		return errors.New("unknown operation")
	}
	if r.Object != (Object{ID: r.Object.ID}) {
		return errors.New("unexpected object metadata")
	}
	return nil
}

func encodeRequest(r request) (string, error) {
	if err := r.validate(); err != nil {
		return "", err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	return protocol + " " + base64.RawURLEncoding.EncodeToString(b), nil
}

func parseRequest(command string) (request, error) {
	var r request
	parts := strings.Split(command, " ")
	if len(command) > maxMetadata*2 || len(parts) != 2 || parts[0] != protocol {
		return r, errors.New("invalid forced-command protocol")
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil {
		return r, fmt.Errorf("request encoding: %w", err)
	}
	if base64.RawURLEncoding.EncodeToString(b) != parts[1] {
		return r, errors.New("noncanonical request encoding")
	}
	if err := decode(bytes.NewReader(b), &r); err != nil {
		return r, err
	}
	return r, r.validate()
}
