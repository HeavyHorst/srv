package service

import (
	"context"
	"testing"
	"time"

	"srv/internal/model"
)

func TestProjectTemplateCannotReadAnotherOwner(t *testing.T) {
	ctx := context.Background()
	st := newServiceTestStore(t)
	inst := serviceTestInstance("private-source", model.StateStopped, time.Now().UTC())
	inst.CreatedByUser = "alice@example.com"
	if err := st.CreateInstance(ctx, inst); err != nil {
		t.Fatal(err)
	}
	app := &App{store: st}
	// A nil provisioner makes any attempt to clone before authorization fail.
	result, err := app.dispatch(ctx, model.Actor{UserLogin: "service:amp-orbs"}, commandRequest{
		args: []string{"project-template", "create", inst.Name, "prepared"}, format: outputFormatJSON,
	})
	if err == nil || result.exitCode == 0 {
		t.Fatal("provider cloned another owner's disk")
	}
}
