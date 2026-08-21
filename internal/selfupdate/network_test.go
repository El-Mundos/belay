package selfupdate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// belayNets is the real shape that produced the bug: Belay on three networks, exactly one of which
// carries the socket-proxy it reaches Docker through.
func belayNets() inspectDoc {
	var d inspectDoc
	d.NetworkSettings.Networks = map[string]struct{ Aliases []string }{
		"traefik":               {},
		"monitoring_monitoring": {},
		"belay_internal":        {},
	}
	return d
}

// stubDocker puts a fake `docker` on PATH that answers the one question helperNetwork asks: which
// networks is the socket-proxy on? Everything else exits non-zero, so a test cannot accidentally
// depend on a real daemon.
func stubDocker(t *testing.T, sockproxyNets string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = inspect ] && [ \"$2\" = belay-sockproxy ]; then printf '%s' '" + sockproxyNets + "'; exit 0; fi\n" +
		"exit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// The original bug in one test: the helper joined whatever network Go's map iteration happened to
// yield, so it reached its socket-proxy roughly one time in three and otherwise failed with a DNS
// error that nothing reported. The choice must come from the transport, not from iteration order.
func TestHelperNetwork_PicksTheNetworkThatReachesDocker(t *testing.T) {
	// The proxy is deliberately placed on the LAST network alphabetically. Sorting alone would
	// answer "belay_internal", so this can only pass if the choice actually comes from inspecting
	// the transport — it distinguishes the fix from its own fallback.
	stubDocker(t, "traefik ")
	m := &Manager{container: "belay", image: "belay:latest", dockerHost: "tcp://belay-sockproxy:2375"}

	// Repeat: a map-order bug passes intermittently, so a single run proves nothing.
	for i := 0; i < 50; i++ {
		if got := m.helperNetwork(context.Background(), sortedNetworks(belayNets())); got != "traefik" {
			t.Fatalf("run %d: helper network = %q, want traefik (where the socket-proxy is)", i, got)
		}
	}
}

// With no DOCKER_HOST the helper gets the socket bind-mounted instead, and joining a network would
// be meaningless.
func TestHelperNetwork_SocketMountNeedsNoNetwork(t *testing.T) {
	m := &Manager{container: "belay", image: "belay:latest"}
	if got := m.helperNetwork(context.Background(), sortedNetworks(belayNets())); got != "" {
		t.Fatalf("helper network = %q, want empty", got)
	}
}

// When the transport cannot be inspected we still have to choose something. It must be the SAME
// something every time: a wrong-but-stable choice is diagnosable, a wrong-and-random one presents as
// flakiness — which is precisely what made the original bug survive so long.
func TestHelperNetwork_FallbackIsDeterministic(t *testing.T) {
	stubDocker(t, "") // every docker call fails
	m := &Manager{container: "belay", image: "belay:latest", dockerHost: "tcp://belay-sockproxy:2375"}
	first := m.helperNetwork(context.Background(), sortedNetworks(belayNets()))
	if first != "belay_internal" {
		t.Fatalf("fallback = %q, want the first sorted network belay_internal", first)
	}
	for i := 0; i < 50; i++ {
		if got := m.helperNetwork(context.Background(), sortedNetworks(belayNets())); got != first {
			t.Fatalf("run %d: fallback = %q, want stable %q", i, got, first)
		}
	}
}

// The replacement Belay is only on its primary network until the helper connects the rest, so it
// must start on the one carrying its transport — and the remaining networks must all still be
// attached, in a stable order.
func TestRecreateScript_StartsOnTheChosenNetworkAndConnectsTheRest(t *testing.T) {
	d := belayNets()
	d.Name = "/belay"
	d.Config.Image = "belay:latest"

	s := recreateScript(d, "belay-previous", "belay:latest", true, "belay_internal")

	if !strings.Contains(s, "--network belay_internal") {
		t.Fatalf("new container does not start on the transport network:\n%s", s)
	}
	for _, n := range []string{"monitoring_monitoring", "traefik"} {
		if !strings.Contains(s, "docker network connect "+n+" belay") {
			t.Fatalf("network %s never reconnected:\n%s", n, s)
		}
	}
	if strings.Contains(s, "docker network connect belay_internal") {
		t.Fatalf("primary network connected twice:\n%s", s)
	}
	// Deterministic output: the same input must render the same script, or it is neither diffable
	// nor testable.
	for i := 0; i < 20; i++ {
		if again := recreateScript(d, "belay-previous", "belay:latest", true, "belay_internal"); again != s {
			t.Fatal("recreateScript is not deterministic across runs")
		}
	}
}

// --- stalled detection ------------------------------------------------------------------------
//
// Reconcile can only speak at startup, which is the one moment a failed swap never reaches: if the
// helper never replaces Belay, Belay never restarts. Stalled is the check that covers that gap, so
// these pin exactly when it is allowed to accuse.

func stalledDocker(t *testing.T, runningImage string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = inspect ]; then printf '%s' '" + runningImage + "'; exit 0; fi\n" +
		"exit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func applyingJournal(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := saveState(dir, State{
		Phase: PhaseApplying, Container: "belay", Backup: "belay-previous",
		FromImage: "sha256:old", FromVersion: "0.2.17", ToImage: "ghcr.io/el-mundos/belay:latest",
	}); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The failure this whole change exists for: the helper never touched us, so we are still the image
// we were, and nobody else is ever going to notice.
func TestStalled_ReportsAnUpdateThatNeverHappened(t *testing.T) {
	stalledDocker(t, "sha256:old")
	m := &Manager{container: "belay", image: "belay:latest"}
	st, stalled := m.Stalled(context.Background(), applyingJournal(t))
	if !stalled {
		t.Fatal("a helper that never replaced Belay must be reported as stalled")
	}
	if st.FromVersion != "0.2.17" {
		t.Errorf("FromVersion = %q, want 0.2.17 (needed for the History row)", st.FromVersion)
	}
}

// We are the NEW image: the swap worked and this process is moments from being torn down. Accusing
// here would file a failure for an update that succeeded.
func TestStalled_SilentWhenTheSwapWorked(t *testing.T) {
	stalledDocker(t, "sha256:new")
	m := &Manager{container: "belay", image: "belay:latest"}
	if _, stalled := m.Stalled(context.Background(), applyingJournal(t)); stalled {
		t.Fatal("a completed swap must not be reported as stalled")
	}
}

// Docker is unreachable, so we cannot tell which image we are. Judge nothing rather than guess —
// the same rule Reconcile follows.
func TestStalled_SilentWhenItCannotTell(t *testing.T) {
	stalledDocker(t, "") // inspect prints nothing but exits 0 => empty, not equal to FromImage
	dir := t.TempDir()
	m := &Manager{container: "belay", image: "belay:latest"}
	if _, stalled := m.Stalled(context.Background(), dir); stalled {
		t.Fatal("an empty journal is not a stalled update")
	}
}

// Abandon must leave nothing behind that would make the next attempt look like it inherited an
// update already in flight.
func TestAbandon_ClearsTheJournal(t *testing.T) {
	stalledDocker(t, "sha256:old")
	dir := applyingJournal(t)
	m := &Manager{container: "belay", image: "belay:latest"}
	m.Abandon(context.Background(), dir)
	if st := LoadState(dir); st.Phase != "" {
		t.Fatalf("journal still at phase %q after Abandon", st.Phase)
	}
	if _, stalled := m.Stalled(context.Background(), dir); stalled {
		t.Fatal("an abandoned update must not be re-reported on the next check")
	}
}
