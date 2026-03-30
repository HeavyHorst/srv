package model

import "time"

const (
	StateProvisioning    = "provisioning"
	StateReady           = "ready"
	StateStopped         = "stopped"
	StateFailed          = "failed"
	StateAwaitingTailnet = "awaiting-tailnet"
	StateDeleting        = "deleting"
	StateDeleted         = "deleted"
)

type Actor struct {
	UserLogin   string
	DisplayName string
	NodeName    string
	RemoteAddr  string
	SSHUser     string
}

type Instance struct {
	ID              string
	Name            string
	State           string
	CreatedAt       time.Time
	CreatedByUser   string
	CreatedByNode   string
	VCPUCount       int64
	MemoryMiB       int64
	RootFSSizeBytes int64
	RootFSPath      string
	KernelPath      string
	InitrdPath      string
	SocketPath      string
	LogPath         string
	SerialLogPath   string
	TapDevice       string
	GuestMAC        string
	NetworkCIDR     string
	HostAddr        string
	GuestAddr       string
	GatewayAddr     string
	FirecrackerPID  int
	TailscaleName   string
	TailscaleIP     string
	LastError       string
	DeletedAt       *time.Time
	UpdatedAt       time.Time
}

type InstanceEvent struct {
	ID         int64
	InstanceID string
	CreatedAt  time.Time
	Type       string
	Message    string
	Payload    string
}

type CommandAudit struct {
	CreatedAt        time.Time
	ActorUser        string
	ActorDisplayName string
	ActorNode        string
	RemoteAddr       string
	SSHUser          string
	Command          string
	ArgsJSON         string
	Allowed          bool
	Reason           string
	DurationMS       int64
	ErrorText        string
}

type AuthzDecision struct {
	CreatedAt  time.Time
	ActorUser  string
	ActorNode  string
	RemoteAddr string
	Action     string
	Allowed    bool
	Reason     string
}
