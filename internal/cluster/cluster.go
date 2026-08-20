// Package cluster defines the small wire protocol between a Belay server and remote agents.
//
// The agent DIALS OUT to the server (so the agent host opens no inbound ports): it registers, then
// long-polls for commands, runs them locally with the full safe-update engine (health-gate +
// snapshot + rollback), and posts results back. All requests carry a shared bearer token.
package cluster

// Service is one compose service on a host.
//
// Protected is the agent's own verdict that a service must not be updated through the ordinary
// path — it is the agent's container, or the Docker transport it depends on. Only the agent can
// know this: the server sees a service name, not which process on that host is executing the
// update. Empty means safe (and is also what an agent too old to send it reports).
type Service struct {
	Name      string `json:"name"`
	Image     string `json:"image"`
	Protected string `json:"protected,omitempty"`
}

// Project is one compose stack on a host.
type Project struct {
	Name     string    `json:"name"`
	File     string    `json:"file"`
	Services []Service `json:"services"`
}

// Registration is what an agent sends on connect / heartbeat.
//
// Version lets the server notice an agent running older code. The protocol tolerates skew on
// purpose — an agent may be unreachable for a while, and encoding/json drops fields it doesn't know
// — but that tolerance is silent: an old agent handed a Command with a field it predates ignores it
// and fails in some unrelated-looking way later. Reporting the version turns that into something a
// human can see. Empty means an agent from before this field existed.
type Registration struct {
	Host     string    `json:"host"`
	Version  string    `json:"version,omitempty"`
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

// Command kinds.
const (
	KindUpdate = "update"     // update one service on the agent's host
	KindSelf   = "selfupdate" // replace the agent's own container, via the helper handoff
)

// Command is a unit of work the server hands an agent.
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
