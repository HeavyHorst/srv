package model

import (
	"strings"
	"time"
)

const (
	StateProvisioning    = "provisioning"
	StateReady           = "ready"
	StateStopped         = "stopped"
	StateFailed          = "failed"
	StateAwaitingTailnet = "awaiting-tailnet"
	StateDeleting        = "deleting"
	StateDeleted         = "deleted"

	MemoryModeFixed = "fixed"
	MemoryModePool  = "pool"
)

type Actor struct {
	UserLogin   string
	DisplayName string
	NodeName    string
	RemoteAddr  string
	SSHUser     string
}

func NormalizeMemoryMode(mode string) string {
	if strings.EqualFold(strings.TrimSpace(mode), MemoryModePool) {
		return MemoryModePool
	}
	return MemoryModeFixed
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
	MemoryMode      string
	MemoryPoolID    string
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

func (i Instance) NormalizedMemoryMode() string {
	return NormalizeMemoryMode(i.MemoryMode)
}

func (i Instance) UsesMemoryPool() bool {
	return i.NormalizedMemoryMode() == MemoryModePool && strings.TrimSpace(i.MemoryPoolID) != ""
}

type MemoryPool struct {
	ID            string
	Name          string
	ReservedBytes int64
	CreatedAt     time.Time
	UpdatedAt     time.Time
	CreatedByUser string
	CreatedByNode string
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
