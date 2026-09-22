package sandbox

// Actor attributes one audit record to the caller that caused it. TenantID and
// AgentIdentity are derived from the sandbox's persisted spec, never from the
// request; KeyID is set only for request-driven actions.
type Actor struct {
	TenantID      string `json:"tenant_id,omitempty"`
	AgentIdentity string `json:"agent_identity,omitempty"`
	KeyID         string `json:"key_id,omitempty"`
}
