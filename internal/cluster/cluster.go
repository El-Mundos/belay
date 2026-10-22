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
//
// Caps lists the command kinds beyond "update" this agent understands. It exists because an agent
// treats any kind it does not know as an update — so the server must never send a newer kind to an
// agent that has not said it can take one.
type Registration struct {
	Host     string    `json:"host"`
	Version  string    `json:"version,omitempty"`
	Caps     []string  `json:"caps,omitempty"`
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
	KindPrune  = "prune"      // remove an image an earlier update left unused (needs CapPrune)
)

// CapPrune is the capability an agent advertises when it can run KindPrune.
const CapPrune = "prune"

// Command is a unit of work the server hands an agent.
type Command struct {
	ID      string        `json:"id"`
	Kind    string        `json:"kind"`
	Project string        `json:"project"` // compose file path on the agent host
	Service string        `json:"service"`
	Image   string        `json:"image"`          // target image ref
	Auth    *RegistryAuth `json:"auth,omitempty"` // scoped pull credential for Image's registry, if private

	// PullBytes is Image's compressed size, for the agent's pre-pull disk check (0 = unknown).
	PullBytes int64 `json:"pull_bytes,omitempty"`

	// KindPrune only: the image to remove and the repository its tags must belong to. Image stays
	// empty on these, so an agent that somehow took one for an update would find nothing to do.
	ImageID string `json:"image_id,omitempty"`
	Repo    string `json:"repo,omitempty"`
}

// PruneReport is an agent's answer to a KindPrune command, and also how an update result names the
// image it left unused (ImageID + Repo only) so the server can queue its removal.
type PruneReport struct {
	ImageID string   `json:"image_id"`
	Repo    string   `json:"repo,omitempty"`
	Removed []string `json:"removed,omitempty"`
	Bytes   int64    `json:"bytes,omitempty"`
	Gone    bool     `json:"gone,omitempty"`
	Kept    string   `json:"kept,omitempty"`
	Retry   bool     `json:"retry,omitempty"`
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

	// Leftover names the image an update left unused (see agent.Leftover); Prune carries the
	// outcome of a KindPrune command instead of an update.
	Leftover *PruneReport `json:"leftover,omitempty"`
	Prune    *PruneReport `json:"prune,omitempty"`
}
