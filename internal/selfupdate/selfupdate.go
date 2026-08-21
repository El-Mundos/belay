// Package selfupdate lets Belay update its own container (Watchtower-style): it detects the belay
// container it runs in, notices when the image tag it was started from now resolves to a newer image
// (e.g. after a `docker load` or registry pull), and recreates itself from its own inspected config.
//
// A container can't cleanly replace itself, so Apply launches a short-lived detached helper container
// (the same belay image, run as `sh`) that outlives belay: it pulls, removes the old container, and
// re-runs it with identical env/mounts/ports/networks but the new image.
package selfupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// Manager performs self-update for the belay container it is running inside.
type Manager struct {
	container  string   // own container name (no leading slash)
	image      string   // own image ref, e.g. "belay:latest"
	networks   []string // every docker network Belay is attached to, sorted (see helperNetwork)
	dockerHost string   // DOCKER_HOST from own env ("" => use the mounted /var/run/docker.sock)
}

// inspectDoc is the subset of `docker inspect` we need to faithfully recreate the container.
type inspectDoc struct {
	Name   string
	Image  string
	Config struct {
		Env    []string
		Cmd    []string
		Image  string
		Labels map[string]string
	}
	HostConfig struct {
		RestartPolicy struct {
			Name              string
			MaximumRetryCount int
		}
		PortBindings map[string][]struct{ HostIp, HostPort string }
	}
	Mounts []struct {
		Type, Name, Source, Destination string
		RW                              bool
	}
	NetworkSettings struct {
		Networks map[string]struct{ Aliases []string }
	}
}

// Detect finds the belay container we're running in (its hostname is the container id by default).
// It returns a disabled (nil) manager without error when we can't self-identify (e.g. not in Docker).
func Detect(ctx context.Context) *Manager {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return nil
	}
	raw, err := exec.CommandContext(ctx, "docker", "inspect", host).Output()
	if err != nil {
		return nil
	}
	var docs []inspectDoc
	if json.Unmarshal(raw, &docs) != nil || len(docs) == 0 {
		return nil
	}
	d := docs[0]
	m := &Manager{
		container: strings.TrimPrefix(d.Name, "/"),
		image:     d.Config.Image,
	}
	m.networks = sortedNetworks(d)
	for _, e := range d.Config.Env {
		if strings.HasPrefix(e, "DOCKER_HOST=") {
			m.dockerHost = strings.TrimPrefix(e, "DOCKER_HOST=")
		}
	}
	return m
}

func (m *Manager) Enabled() bool { return m != nil && m.container != "" }

// Image is Belay's own image ref ("" when not running in a container). Nil-safe: Detect returns nil
// outside a container, and callers hold that nil directly.
func (m *Manager) Image() string {
	if m == nil {
		return ""
	}
	return m.image
}

// NewForTest builds a Manager with a known identity, for callers that need to exercise logic keyed
// on Belay's own container/image/transport without a live Docker daemon to detect them from.
func NewForTest(container, image, dockerHost string) *Manager {
	return &Manager{container: container, image: image, dockerHost: dockerHost}
}

// Container is Belay's own container name ("" when not running in one).
func (m *Manager) Container() string {
	if m == nil {
		return ""
	}
	return m.container
}

// DockerHost is the DOCKER_HOST Belay talks to ("" => the mounted socket). Its hostname names the
// container Belay depends on for all Docker access — typically a socket-proxy.
func (m *Manager) DockerHost() string {
	if m == nil {
		return ""
	}
	return m.dockerHost
}

// Protects reports why a service must not be updated by the Belay this Manager belongs to, or ""
// when it is safe. Two services in any Belay deployment cannot be recreated by the very Belay doing
// the recreating:
//
//   - Belay's own container. `docker compose up -d belay` kills the process mid-update, so the
//     update never finishes and nothing rolls it back. Apply exists precisely for this, handing off
//     to a helper container that outlives Belay's death.
//   - Whatever DOCKER_HOST points at. Reaching Docker to recreate the socket-proxy requires the
//     socket-proxy, so the moment it stops, the operation loses the connection it was travelling
//     over and can neither finish nor revert. Belay saws off the branch it is sitting on.
//
// This lives here, not in the server, because it is a property of a Belay PROCESS and its host —
// which makes it equally true of an agent. An agent answers for its own host: the server cannot
// know which container is the agent on a machine it only talks to over the wire.
func (m *Manager) Protects(service, image string) string {
	if m == nil {
		return ""
	}
	if m.container != "" && service == m.container {
		return "Belay itself — use self-update, which hands off to a helper container"
	}
	if m.image != "" && image == m.image {
		return "Belay's own image — use self-update"
	}
	if h := dockerHostName(m.dockerHost); h != "" && service == h {
		return "Belay reaches Docker through this service — recreating it would cut the connection mid-update"
	}
	return ""
}

// sortedNetworks lists a container's networks in a stable order. Go randomises map iteration, and
// every use of this set here feeds something that must not change shape between runs.
func sortedNetworks(d inspectDoc) []string {
	out := make([]string, 0, len(d.NetworkSettings.Networks))
	for n := range d.NetworkSettings.Networks {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// helperNetwork picks the ONE network the helper container must join in order to reach Docker.
//
// This is not a cosmetic choice. Belay routinely sits on several networks at once — an internal one
// carrying its socket-proxy, plus a proxy network for Traefik, plus whatever else it was joined to
// for notifications — and only ONE of them can resolve DOCKER_HOST. A helper launched on any of the
// others comes up unable to resolve its transport, so every docker command it runs fails with
// "lookup belay-sockproxy on 127.0.0.11:53: no such host", the swap never happens, and Belay simply
// keeps running the old image.
//
// The previous implementation took whatever name Go's randomised map iteration yielded first, which
// made self-update a dice roll whose odds were 1/len(networks). It presented as flakiness — some
// updates worked, some silently did nothing — rather than as the deterministic bug it was.
//
// So ask the transport itself which networks it is on and take the overlap, rather than guessing.
// nets must already be sorted, so that the fallback (and therefore the failure) is at least
// reproducible.
func (m *Manager) helperNetwork(ctx context.Context, nets []string) string {
	if m.dockerHost == "" {
		return "" // the socket is bind-mounted into the helper instead; no network needed
	}
	if len(nets) == 0 {
		return ""
	}
	if host := dockerHostName(m.dockerHost); host != "" {
		out, err := dockerOut(ctx, "inspect", host, "--format",
			`{{range $n, $v := .NetworkSettings.Networks}}{{$n}} {{end}}`)
		if err == nil {
			on := make(map[string]bool, 4)
			for _, n := range strings.Fields(out) {
				on[n] = true
			}
			for _, n := range nets {
				if on[n] {
					return n
				}
			}
		}
	}
	// DOCKER_HOST names something we cannot inspect (an IP, a host outside Docker, a stopped
	// proxy). A single sorted choice is still better than a random one: if it is wrong, it is
	// wrong the same way every time, which is what makes it diagnosable.
	return nets[0]
}

// dockerHostName pulls the container name out of a DOCKER_HOST like "tcp://belay-sockproxy:2375".
func dockerHostName(dockerHost string) string {
	if dockerHost == "" {
		return ""
	}
	h := dockerHost
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	h, _, _ = strings.Cut(h, ":")
	return h
}

// Available reports whether the image tag now resolves to a different image than the running one.
// For registry images it first attempts a pull; a failed pull (e.g. a local-only tag) is ignored.
func (m *Manager) Available(ctx context.Context) (bool, error) {
	if !m.Enabled() {
		return false, nil
	}
	_ = exec.CommandContext(ctx, "docker", "pull", m.image).Run() // best-effort; local-only tags fail
	tagID, err := dockerOut(ctx, "image", "inspect", m.image, "--format", "{{.Id}}")
	if err != nil {
		return false, err
	}
	runningID, err := dockerOut(ctx, "inspect", m.container, "--format", "{{.Image}}")
	if err != nil {
		return false, err
	}
	return tagID != "" && tagID != runningID, nil
}

// TargetVersion is the version of the image the tracked tag currently resolves to -- the version an
// update WOULD move to. It is read from the image's OCI label rather than by running the binary:
// the image is already pulled, and starting a container just to ask it its version is a silly price
// for a line of text. Empty when the label is absent (an image built without it).
func (m *Manager) TargetVersion(ctx context.Context) string {
	if !m.Enabled() {
		return ""
	}
	out, err := dockerOut(ctx, "image", "inspect", m.image,
		"--format", `{{index .Config.Labels "org.opencontainers.image.version"}}`)
	if err != nil {
		return ""
	}
	v := strings.TrimSpace(out)
	if v == "<no value>" { // docker renders a missing label this way
		return ""
	}
	return v
}

// helperName is the fixed name of the throwaway container that performs the swap. Fixed, not
// random, so a second attempt replaces the first rather than racing it, and so any Belay can find
// and reap a finished one.
const helperName = "belay-selfupdate"

// Apply recreates belay from a fresh inspection, using the current image tag. It returns once the
// detached helper has been launched — belay itself is torn down and replaced moments later.
//
// dir is the data directory holding the journal (see journal.go); passing "" runs without one,
// which still updates but leaves nobody able to report what happened if it goes wrong.
// previousTag is the local tag kept on the image Belay was running before an update. A tag is the
// only thing that keeps that image out of reach of `docker image prune`: once :latest moves, the
// build you were running becomes dangling and one prune away from being unrecoverable — and it is
// the only thing a manual rollback can go back to.
const previousTag = "belay:previous"

// Opts configures an update. Grouped rather than passed positionally because two of these are
// strings whose order nobody would remember.
type Opts struct {
	Dir         string        // data directory holding the journal ("" = no journal, no reporting)
	FromVersion string        // the version we are leaving, recorded for History and rollback
	Window      time.Duration // how long a manual rollback stays on offer
	FollowUp    string        // work to run after this update is confirmed healthy
}

// Apply replaces Belay with the current image tag. See applyImage.
func (m *Manager) Apply(ctx context.Context, o Opts) error {
	return m.applyImage(ctx, o, m.image, true)
}

// Rollback undoes a completed self-update, putting back the image Belay was running before it.
//
// This is a separate path from the helper's automatic rollback, which only covers the gate window
// (seconds) and works by restoring the previous CONTAINER. By the time a human decides an update
// was a mistake, that container is long reaped — so this rebuilds from the retained IMAGE instead.
func (m *Manager) Rollback(ctx context.Context, dir string) error {
	st := LoadState(dir)
	rb := st.Rollback
	if rb == nil {
		return fmt.Errorf("no self-update to roll back")
	}
	if time.Now().After(rb.Until) {
		return fmt.Errorf("the rollback window for this self-update has passed")
	}
	if _, err := dockerOut(ctx, "image", "inspect", rb.Image, "--format", "{{.Id}}"); err != nil {
		return fmt.Errorf("the previous image is no longer on this host (pruned?)")
	}
	// Re-point the tracked ref at the old build and recreate from it WITHOUT pulling. Running the
	// bare image id instead would leave the container tracking an id rather than a tag, and Belay
	// would never notice a future release; this way the next check pulls, sees the newer build, and
	// offers it again — a rollback, not a dead end.
	if err := runDocker(ctx, "tag", rb.Image, m.image); err != nil {
		return fmt.Errorf("re-tag previous image: %w", err)
	}
	// A rollback carries no follow-up: the point is to stop, not to continue a rollout.
	return m.applyImage(ctx, Opts{Dir: dir, FromVersion: rb.Version, Window: time.Until(rb.Until)}, m.image, false)
}

// applyImage recreates Belay's container onto target, handing off to a helper that outlives this
// process. It returns once the helper is launched — Belay is torn down moments later.
//
// dir is the data directory holding the journal (see journal.go); passing "" runs without one,
// which still updates but leaves nobody able to report what happened if it goes wrong.
func (m *Manager) applyImage(ctx context.Context, o Opts, target string, pull bool) error {
	dir := o.Dir
	if !m.Enabled() {
		return fmt.Errorf("self-update not available (not running in a detectable container)")
	}
	raw, err := exec.CommandContext(ctx, "docker", "inspect", m.container).Output()
	if err != nil {
		return err
	}
	var docs []inspectDoc
	if err := json.Unmarshal(raw, &docs); err != nil || len(docs) == 0 {
		return fmt.Errorf("inspect self: %v", err)
	}
	// The image ID we are running now — the journal's test for "am I still the old Belay?", and
	// the thing a later manual rollback goes back to.
	runningID, err := dockerOut(ctx, "inspect", m.container, "--format", "{{.Image}}")
	if err != nil {
		return fmt.Errorf("inspect self image: %w", err)
	}
	// Keep the outgoing image reachable by name before anything moves the tag off it.
	_ = runDocker(ctx, "tag", runningID, previousTag)

	backup := m.container + "-previous"
	if err := saveState(dir, State{
		Phase: PhaseApplying, Container: m.container, Backup: backup,
		FromImage: runningID, FromVersion: o.FromVersion, ToImage: target,
		Window: o.Window, FollowUp: o.FollowUp, Started: time.Now(),
	}); err != nil {
		return fmt.Errorf("write self-update journal: %w", err)
	}
	// The network that reaches Docker, chosen once and used for BOTH the helper and the Belay it
	// recreates: the new container is only on its primary network until the helper connects the
	// rest, so starting it anywhere else means booting blind to its own transport.
	net := m.helperNetwork(ctx, sortedNetworks(docs[0]))
	script := recreateScript(docs[0], backup, target, pull, net)

	// A leftover helper from a previous attempt would hold the name; it has already finished.
	_ = runDocker(ctx, "rm", "-f", helperName)

	// NOT --rm, and restartable: if the helper dies mid-swap (host reboot, OOM) Docker starts it
	// again and the script — which is idempotent — finishes or rolls back. A --rm helper that died
	// between removing the old container and starting the new one left nothing running and nothing
	// that would ever retry.
	args := []string{"run", "-d", "--name", helperName, "--restart", "on-failure:5"}
	if net != "" {
		args = append(args, "--network", net)
	}
	if m.dockerHost != "" {
		args = append(args, "-e", "DOCKER_HOST="+m.dockerHost)
	} else {
		args = append(args, "-v", "/var/run/docker.sock:/var/run/docker.sock")
	}
	// same belay image, run as a shell; sleep briefly so belay can answer the HTTP request first
	args = append(args, "--entrypoint", "sh", target, "-c", "sleep 3; "+script)
	if err := exec.CommandContext(ctx, "docker", args...).Run(); err != nil {
		clearState(dir) // nothing was launched, so nothing is in flight
		return err
	}
	return nil
}

// recreateScript builds the shell the helper runs: swap the container onto the current image tag,
// verify it stays up, and put the old one back if it does not.
//
// Three properties matter more than brevity here, because nothing supervises this script:
//
//   - It RENAMES the old container instead of removing it. That container, stopped and intact, is
//     the rollback. `docker rm -f` destroyed the only way back before the replacement had proven
//     it could run.
//   - It is IDEMPOTENT. Docker restarts the helper after a crash, so the script must be able to
//     run again from any point: it exits immediately if the target is already up on the new image.
//   - It GATES on the new container still running after a wait, and rolls back if not — a
//     crash-looping new image no longer leaves the host with no Belay.
//
// pull controls whether the helper refreshes the image first. An update must (the whole point is
// to fetch a newer build); a rollback must NOT, because the tag has deliberately been re-pointed at
// the older image locally and pulling would undo that immediately.
//
// primary is the network the new container starts on (see helperNetwork); the remaining networks
// are connected immediately afterwards. "" falls back to the first sorted network.
func recreateScript(d inspectDoc, backup, image string, pull bool, primary string) string {
	name := strings.TrimPrefix(d.Name, "/")

	run := []string{"docker", "run", "-d", "--name", name}
	if p := d.HostConfig.RestartPolicy.Name; p != "" && p != "no" {
		run = append(run, "--restart", p)
	}
	for _, e := range d.Config.Env {
		run = append(run, "-e", e)
	}
	// Labels are not decoration: they are how the container is IDENTIFIED and ROUTED. A Belay
	// deployed by Compose carries com.docker.compose.* (drop them and the container is orphaned
	// from its own stack — `compose up -d` no longer sees it, and would try to create a second one
	// against the name), and behind Traefik it carries the router and forward-auth rules that put
	// it on the internet. Recreating without them silently takes the UI offline and detaches it
	// from the stack file, which is the opposite of what an update should do.
	for _, k := range sortedKeys(d.Config.Labels) {
		run = append(run, "--label", k+"="+d.Config.Labels[k])
	}
	for _, mnt := range d.Mounts {
		src := mnt.Source
		if mnt.Type == "volume" {
			src = mnt.Name
		}
		spec := src + ":" + mnt.Destination
		if !mnt.RW {
			spec += ":ro"
		}
		run = append(run, "-v", spec)
	}
	for cp, binds := range d.HostConfig.PortBindings {
		for _, b := range binds {
			hostpart := b.HostPort
			if b.HostIp != "" {
				hostpart = b.HostIp + ":" + b.HostPort
			}
			run = append(run, "-p", hostpart+":"+cp)
		}
	}
	// Primary network on `run`, the rest connected afterwards. Sorted rather than map-ordered so the
	// generated script is deterministic — the same property sortedKeys gives the labels.
	nets := sortedNetworks(d)
	first := primary
	if first == "" && len(nets) > 0 {
		first = nets[0]
	}
	var rest []string
	for _, n := range nets {
		if n != first {
			rest = append(rest, n)
		}
	}
	if first != "" {
		run = append(run, "--network", first)
	}
	run = append(run, image)
	run = append(run, d.Config.Cmd...)

	qname, qbackup, qimage := shq(name), shq(backup), shq(image)
	running := "docker inspect -f '{{.State.Running}}' " + qname + " 2>/dev/null"

	var b strings.Builder
	b.WriteString("set -e\n")
	// PULL FIRST. The "already done?" check below compares the tag against the running container,
	// and on a moving tag like :latest the local copy is usually stale -- an agent never pulls on
	// its own. Checking before pulling therefore compares yesterday's image against itself, decides
	// the work is done, and exits 0 having updated nothing.
	if pull {
		b.WriteString("docker pull " + qimage + " 2>/dev/null || true\n")
	}
	// Already done? A restarted helper must not tear down a healthy new container and start over.
	b.WriteString("target=$(docker image inspect -f '{{.Id}}' " + qimage + " 2>/dev/null || echo none)\n")
	b.WriteString("current=$(docker inspect -f '{{.Image}}' " + qname + " 2>/dev/null || echo none)\n")
	b.WriteString("if [ \"$target\" = \"$current\" ] && [ \"$(" + running + ")\" = \"true\" ]; then exit 0; fi\n")
	// Keep the old container as the rollback target instead of destroying it.
	b.WriteString("docker rm -f " + qbackup + " 2>/dev/null || true\n")
	b.WriteString("docker stop " + qname + " 2>/dev/null || true\n")
	b.WriteString("docker rename " + qname + " " + qbackup + " 2>/dev/null || true\n")
	b.WriteString("if " + shellJoin(run) + "; then\n")
	for _, n := range rest {
		b.WriteString("  docker network connect " + shq(n) + " " + qname + " || true\n")
	}
	// Gate: the replacement must still be running after the window. This catches a crash-loop,
	// which is the failure a bad image actually produces.
	b.WriteString(fmt.Sprintf("  sleep %d\n", int(GateWindow.Seconds())))
	b.WriteString("  if [ \"$(" + running + ")\" = \"true\" ]; then exit 0; fi\n")
	b.WriteString("fi\n")
	// Rollback: discard the failed replacement and put the previous container back.
	b.WriteString("docker rm -f " + qname + " 2>/dev/null || true\n")
	b.WriteString("docker rename " + qbackup + " " + qname + "\n")
	b.WriteString("docker start " + qname + "\n")
	return b.String()
}

// sortedKeys gives map iteration a stable order, so the generated script is deterministic (and
// therefore diffable and testable) rather than reshuffling on every run.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// runDocker runs a docker command for its effect, discarding output.
func runDocker(ctx context.Context, args ...string) error {
	return exec.CommandContext(ctx, "docker", args...).Run()
}

func dockerOut(ctx context.Context, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", args...).Output()
	return strings.TrimSpace(string(out)), err
}

func shellJoin(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = shq(a)
	}
	return strings.Join(q, " ")
}

// shq quotes a shell argument only when it contains anything outside a safe set — keeps the common
// `docker run -d --name belay …` readable while still quoting values with spaces/specials.
func shq(s string) string {
	if s != "" && strings.IndexFunc(s, unsafeShell) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func unsafeShell(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return false
	case strings.ContainsRune("_@%+=:,./-", r):
		return false
	}
	return true
}
