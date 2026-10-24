// Package orbguest implements the narrow host-to-Orb bootstrap protocol.
package orbguest

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

const Port = 4050
const MaxMessage = 64 * 1024

type Request struct {
	Token      string `json:"token"`
	AMPURL     string `json:"amp_url"`
	Proxy      string `json:"proxy"`
	Generation int64  `json:"generation"`
}

func Ping(ctx context.Context, path string) error { return exchange(ctx, path, "ping", nil) }
func Bootstrap(ctx context.Context, path string, req Request) error {
	if req.Generation <= 0 {
		return errors.New("positive bootstrap generation required")
	}
	return exchange(ctx, path, "bootstrap", &req)
}

func exchange(ctx context.Context, path, action string, req *Request) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	data, err := json.Marshal(struct {
		Action string `json:"action"`
		*Request
	}{action, req})
	if err != nil || len(data)+1 > MaxMessage {
		return errors.New("invalid orb request")
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return fmt.Errorf("connect orb transport: %w", err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", Port); err != nil {
		return err
	}
	reader := bufio.NewReaderSize(conn, MaxMessage)
	line, err := reader.ReadSlice('\n')
	if err != nil {
		return errors.New("orb handshake failed")
	}
	fields := strings.Fields(string(line))
	if len(fields) != 2 || fields[0] != "OK" {
		return errors.New("orb handshake rejected")
	}
	if _, err := strconv.ParseUint(fields[1], 10, 32); err != nil {
		return errors.New("invalid orb handshake")
	}
	if _, err := conn.Write(append(data, '\n')); err != nil {
		return err
	}
	line, err = reader.ReadSlice('\n')
	if err != nil {
		return errors.New("orb response unavailable or oversized")
	}
	var response struct {
		OK         bool  `json:"ok"`
		Generation int64 `json:"generation"`
	}
	// Never echo remote response text: it may contain credentials.
	if json.Unmarshal(line, &response) != nil || !response.OK {
		return errors.New("orb guest rejected request")
	}
	if req != nil && response.Generation != req.Generation {
		return errors.New("orb guest generation acknowledgment mismatch")
	}
	return nil
}
