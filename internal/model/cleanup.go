package model

import "time"

// CleanupReceipt is emitted only after the host has checked every component and
// committed this record, the request outcome, and the tombstone atomically.
// It contains identifiers, never enrollment credentials.
type CleanupReceipt struct {
	Version            int       `json:"version"`
	BackendID          string    `json:"backend_id"`
	Kind               string    `json:"kind"`
	Image              string    `json:"image"`
	RequestID          string    `json:"request_id"`
	Actor              string    `json:"actor"`
	InstanceID         string    `json:"instance_id"`
	Name               string    `json:"name"`
	CreatedAt          time.Time `json:"created_at"`
	CreatedByUser      string    `json:"created_by_user"`
	CreatedByNode      string    `json:"created_by_node"`
	AuthKeyDisposition string    `json:"auth_key_disposition"`
	RetainedTemplates  []string  `json:"retained_templates"`
	IngressClosed      bool      `json:"ingress_closed"`
	RuntimeAbsent      bool      `json:"runtime_absent"`
	JailerAbsent       bool      `json:"jailer_absent"`
	CgroupAbsent       bool      `json:"cgroup_absent"`
	NetworkAbsent      bool      `json:"network_absent"`
	TapDevice          string    `json:"tap_device"`
	NetworkCIDR        string    `json:"network_cidr"`
	InstanceDir        string    `json:"instance_dir"`
	RootFSPath         string    `json:"rootfs_path"`
	BackupsDir         string    `json:"backups_dir"`
	StorageAbsent      bool      `json:"storage_absent"`
	BackupsAbsent      bool      `json:"backups_absent"`
	SnapshotsAbsent    bool      `json:"snapshots_absent"`
	DeletedAt          time.Time `json:"deleted_at"`
}
