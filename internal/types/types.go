package types

type ReplicaStatus struct {
	ID       string `json:"id"`
	NodeIP   string `json:"node_ip"`
	Port     int    `json:"port"`
	Uptime   string `json:"uptime"`     
	StartedAt int64  `json:"started_at"` 
}

type DeployRequest struct {
	Replicas int `json:"replicas"`
}

type StartServiceRequest struct {
	ServiceID string            `json:"service_id"`
	Port      int               `json:"port"`
	Config    map[string]string `json:"config,omitempty"` 
}