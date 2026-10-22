package model

// Migration is host-local control-plane state, never part of a portable artifact.
// Every phase except aborted permanently fences the source incarnation.
type Migration struct {
	MemoryPoolName string `json:"memory_pool_name,omitempty"`
	FixedMemory    bool   `json:"fixed_memory,omitempty"`
	InstanceID     string `json:"instance_id"`
	Token          string `json:"token"`
	Target         string `json:"target"`
	WasRunning     bool   `json:"was_running"`
	Phase          string `json:"phase"`
}

// ImportReceipt identifies the artifact committed on this host. It is not an
// authorization to un-fence a source and is never exported.
type ImportReceipt struct {
	InstanceID       string `json:"instance_id"`
	Token            string `json:"token"`
	SourceInstanceID string `json:"source_instance_id"`
	ExportedAt       string `json:"exported_at"`
	Phase            string `json:"phase"`
}
