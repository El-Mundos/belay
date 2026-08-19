// Package cluster defines the small wire protocol between a Belay server and remote agents.
//
// The agent DIALS OUT to the server (so the agent host opens no inbound ports): it registers, then
// long-polls for commands, runs them locally with the full safe-update engine (health-gate +
// snapshot + rollback), and posts results back. All requests carry a shared bearer token.
package cluster

// Service is one compose service on a host.
type Service struct {
	Name  string `json:"name"`
	Image string `json:"image"`
}

// Project is one compose stack on a host.
type Project struct {
	Name     string    `json:"name"`
	File     string    `json:"file"`
	Services []Service `json:"services"`
}

// Registration is what an agent sends on connect / heartbeat.
type Registration struct {
	Host     string    `json:"host"`
	Projects []Project `json:"projects"`
}

// RegistryAuth is a single scoped credential for pulling a command's target image from a private
// registry. The server attaches only the credential matching the image's registry (when one is
// configured); a nil Auth means anonymous — or whatever login the agent host already holds.
type RegistryAuth struct {
	Host     string `json:"host"`
	Username string `json:"username"`
	Token    string `json:"token"`
}

// Command is a unit of work the server hands an agent. Kind is currently always "update".
type Command struct {
	ID      string        `json:"id"`
	Kind    string        `json:"kind"`
	Project string        `json:"project"` // compose file path on the agent host
	Service string        `json:"service"`
	Image   string        `json:"image"`          // target image ref
	Auth    *RegistryAuth `json:"auth,omitempty"` // scoped pull credential for Image's registry, if private
}

// Result is what the agent posts back after running a command.
type Result struct {
	CommandID string `json:"command_id"`
	Host      string `json:"host"`
	Project   string `json:"project"` // display name
	Service   string `json:"service"`
	From      string `json:"from"`
	To        string `json:"to"`
	Outcome   string `json:"outcome"`
	Err       string `json:"err"`
	Logs      string `json:"logs"`
	Duration  string `json:"duration"`
}
