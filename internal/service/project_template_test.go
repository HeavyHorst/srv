package service

import (
	"context"
	"strings"
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

func TestProjectTemplateDeleteRejectsUnexpectedTarget(t *testing.T) {
	for _, tc := range []struct{ owner, backend, want string }{
		{"service:other", "backend-one", "owner does not match"},
		{"service:amp-orbs", "backend-two", "backend ID does not match"},
	} {
		app := &App{backendID: tc.backend}
		result, err := app.dispatch(context.Background(), model.Actor{UserLogin: tc.owner}, commandRequest{
			args: []string{"project-template", "delete", "prepared", "--expect-owner", "service:amp-orbs", "--expect-source", "source-id", "--expect-backend", "backend-one"}, format: outputFormatJSON,
		})
		if err == nil || result.exitCode == 0 || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("expected %q before accessing provisioner: %v", tc.want, err)
		}
	}
}
