package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strconv"

	gssh "github.com/gliderlabs/ssh"
	"github.com/google/uuid"
	"srv/internal/model"
	"srv/internal/orbguest"
)

// Credentials are carried only on SSH stdin, never in command/audit arguments.
func (a *App) handleOrbBootstrapSession(sess gssh.Session, actor model.Actor, args []string) (int, error) {
	fail := func(err error) (int, error) {
		_, _ = io.WriteString(sess.Stderr(), err.Error()+"\n")
		return 1, err
	}
	if len(args) != 9 || args[3] != "--expect" {
		return fail(errors.New("usage: orb bootstrap NAME --expect REQUEST_ID UUID CREATED_AT BACKEND_ID IMAGE (JSON credentials and generation on stdin)"))
	}
	if _, err := uuid.Parse(args[4]); err != nil {
		return fail(errors.New("invalid bootstrap request ID"))
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
	if err := a.checkOrbIdentity(sess.Context(), inst, args[5:]); err != nil {
		return fail(err)
	}
	r := orbOperationRequest(actor, "bootstrap", inst.Name, args[4], append(args[5:], strconv.FormatInt(request.Generation, 10)))
	if err := a.store.AdmitBootstrap(sess.Context(), r, inst.ID, inst.CreatedAt, request.Generation); err != nil {
		return fail(err)
	}
	if err := orbguest.Bootstrap(sess.Context(), filepath.Join(a.cfg.InstancesDir(), inst.Name, "orb.vsock"), request); err != nil {
		// Do not relay guest output or credentials into logs.
		return fail(errors.New("orb bootstrap failed; inspect the guest bootstrap service"))
	}
	if err := a.finishOrbOperation(sess.Context(), actor, args[3:]); err != nil {
		return fail(err)
	}
	_, err = io.WriteString(sess, "orb bootstrap started\n")
	return 0, err
}
