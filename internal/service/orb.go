package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"

	gssh "github.com/gliderlabs/ssh"
	"srv/internal/model"
	"srv/internal/orbguest"
)

// Credentials are carried only on SSH stdin, never in command/audit arguments.
func (a *App) handleOrbBootstrapSession(sess gssh.Session, actor model.Actor, args []string) (int, error) {
	fail := func(err error) (int, error) {
		_, _ = io.WriteString(sess.Stderr(), err.Error()+"\n")
		return 1, err
	}
	if len(args) != 3 {
		return fail(errors.New("usage: orb bootstrap NAME (JSON credentials on stdin)"))
	}
	unlock := a.lockInstance(args[2])
	defer unlock()
	inst, err := a.lookupVisibleInstance(sess.Context(), actor, args[2])
	if err != nil {
		return fail(err)
	}
	if inst.KindName() != model.KindOrb || inst.State != model.StateReady {
		return fail(errors.New("bootstrap requires a ready orb"))
	}
	body, err := io.ReadAll(io.LimitReader(sess, 16385))
	if err != nil || len(body) > 16384 {
		return fail(errors.New("invalid orb bootstrap request"))
	}
	var request orbguest.Request
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return fail(errors.New("invalid orb bootstrap request"))
	}
	if decoder.Decode(new(any)) != io.EOF {
		return fail(errors.New("invalid orb bootstrap request"))
	}
	if err := orbguest.Bootstrap(sess.Context(), filepath.Join(filepath.Dir(inst.RootFSPath), "orb.vsock"), request); err != nil {
		// Do not relay guest output or credentials into logs.
		return fail(errors.New("orb bootstrap failed; inspect the guest bootstrap service"))
	}
	_, err = io.WriteString(sess, "orb bootstrap started\n")
	return 0, err
}
