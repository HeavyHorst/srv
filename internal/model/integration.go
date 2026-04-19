package model

import "time"

type IntegrationKind string

const (
	IntegrationKindHTTP IntegrationKind = "http"
)

type IntegrationAuthMode string

const (
	IntegrationAuthNone      IntegrationAuthMode = "none"
	IntegrationAuthBearerEnv IntegrationAuthMode = "bearer_env"
	IntegrationAuthBasicEnv  IntegrationAuthMode = "basic_env"
)

type IntegrationHeader struct {
	Name  string `json:"name"`
	Value string `json:"value,omitempty"`
	Env   string `json:"env,omitempty"`
}

type Integration struct {
	ID               string
	Name             string
	Kind             IntegrationKind
	TargetURL        string
	AuthMode         IntegrationAuthMode
	BearerEnv        string
	BasicUser        string
	BasicPasswordEnv string
	HeadersJSON      string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type InstanceIntegrationBinding struct {
	InstanceID    string
	IntegrationID string
	CreatedAt     time.Time
	CreatedByUser string
	CreatedByNode string
}
