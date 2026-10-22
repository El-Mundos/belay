package agent

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/El-Mundos/belay/internal/engine"
	"github.com/El-Mundos/belay/internal/registry"
)

// keptTags are local tags that hold an image on purpose. belay:previous is the self-update's
// rollback material (see selfupdate.previousTag); the helper image backs every snapshot.
var keptTags = []string{"belay:previous", helperImage}

// ServiceImageID returns the image ID of a service's container, running or stopped — the exact
// image an update is about to replace. Empty when the service has no container or Docker cannot
// say, which leaves nothing to prune afterwards rather than guessing from a tag.
func ServiceImageID(ctx context.Context, file, service string) string {
	out, err := output(ctx, "docker", "compose", "-f", file, "ps", "-aq", service)
	if err != nil {
		return ""
	}
	cid := strings.TrimSpace(strings.SplitN(strings.TrimSpace(out), "\n", 2)[0])
	if cid == "" {
		return ""
	}
	id, err := output(ctx, "docker", "inspect", "-f", "{{.Image}}", cid)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(id)
}

// ImageID resolves a local image reference to its ID ("" if it is not present).
func ImageID(ctx context.Context, ref string) string {
	out, err := output(ctx, "docker", "image", "inspect", "--format", "{{.Id}}", ref)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// RepoOf names the repository an image ref belongs to, normalised the way registries see it, so
// "prom/prometheus:v3" and "docker.io/prom/prometheus:latest" compare equal.
func RepoOf(ref string) string {
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	r := registry.ParseRef(ref)
	return r.Registry + "/" + r.Repository
}

// Leftover returns the image an attempt left unused, which Belay now owns the cleanup of:
//
//   - updated: the image the service ran before (fromID), once the new container is on another;
//   - rolled_back: the target it pulled and then abandoned.
//
// Anything else — skipped, or an error that needs a human — leaves the host as it is.
func Leftover(ctx context.Context, outcome engine.Outcome, file, service, fromID, target string) string {
	cur := ServiceImageID(ctx, file, service)
	switch outcome {
	case engine.OutcomeUpdated:
		if fromID != "" && cur != "" && cur != fromID {
			return fromID
		}
	case engine.OutcomeRolledBack:
		if id := ImageID(ctx, target); id != "" && id != fromID && id != cur {
			return id
		}
	}
	return ""
}

// PruneResult is what one removal attempt did.
type PruneResult struct {
	Removed []string // tags removed, plus the bare ID when the image had none
	Bytes   int64    // free space gained (0 = not measurable)
	Gone    bool     // the image was not on the host any more; nothing to do
	Kept    string   // why it was left alone ("" when it was removed or gone)
	Retry   bool     // the reason may clear (still in use, rollback window) — ask again later
}

// Prune removes one image an update left behind, unless something still needs it.
//
// keep maps image IDs that must survive to the reason why: live rollback points, the self-update
// journal. On top of that the image is kept when any container — running or stopped — uses it,
// when it carries a tag Belay holds on purpose, or when it carries a tag from another repository:
// someone re-tagged it by hand, and it is theirs now. Removal never forces, so the daemon refuses
// on its own if a container appeared in between.
func Prune(ctx context.Context, imageID, repo string, keep map[string]string) PruneResult {
	tags, err := imageTags(ctx, imageID)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such image") {
			return PruneResult{Gone: true}
		}
		return PruneResult{Kept: "could not inspect it: " + err.Error(), Retry: true}
	}
	if why, ok := keep[imageID]; ok {
		return PruneResult{Kept: why, Retry: true}
	}
	users, err := containersUsing(ctx, imageID)
	if err != nil {
		return PruneResult{Kept: "could not list containers: " + err.Error(), Retry: true}
	}
	if len(users) > 0 {
		return PruneResult{Kept: "still used by container " + strings.Join(users, ", "), Retry: true}
	}
	untag, why := pruneTags(tags, repo)
	if why != "" {
		return PruneResult{Kept: why}
	}

	before, errBefore := FreeBytes(ctx)
	var res PruneResult
	for _, t := range untag {
		if err := run(ctx, "docker", "image", "rm", t); err != nil {
			res.Kept, res.Retry = "removing "+t+" failed: "+firstLine(err.Error()), true
			return res
		}
		res.Removed = append(res.Removed, t)
	}
	// Untagging the last tag removes the image itself; what is left to remove by ID is a build with
	// no tags at all — the one a rebase leaves dangling.
	if ImageID(ctx, imageID) != "" {
		if err := run(ctx, "docker", "image", "rm", imageID); err != nil {
			res.Kept, res.Retry = "removing "+shortID(imageID)+" failed: "+firstLine(err.Error()), true
			return res
		}
		if len(untag) == 0 {
			res.Removed = append(res.Removed, shortID(imageID))
		}
	}
	if after, err := FreeBytes(ctx); err == nil && errBefore == nil && after > before {
		res.Bytes = after - before
	}
	return res
}

// pruneTags decides which tags to remove with an image, or why the image must stay. Every tag has
// to belong to repo: an image Belay replaced as prom/prometheus:v3.13.1 may also be :latest (same
// ID, same repository, pulled the same way), but a tag from anywhere else was put there by hand.
func pruneTags(tags []string, repo string) (untag []string, keepWhy string) {
	for _, t := range tags {
		for _, k := range keptTags {
			if t == k {
				return nil, "it is tagged " + t + ", which Belay keeps on purpose"
			}
		}
		if RepoOf(t) != repo {
			return nil, "it is also tagged " + t + ", which Belay did not put there"
		}
		untag = append(untag, t)
	}
	return untag, ""
}

func imageTags(ctx context.Context, imageID string) ([]string, error) {
	out, err := output(ctx, "docker", "image", "inspect", "--format", `{{join .RepoTags "\n"}}`, imageID)
	if err != nil {
		return nil, fmt.Errorf("%s", firstLine(strings.TrimSpace(out)))
	}
	var tags []string
	for _, t := range strings.Split(strings.TrimSpace(out), "\n") {
		if t = strings.TrimSpace(t); t != "" {
			tags = append(tags, t)
		}
	}
	return tags, nil
}

// containersUsing names the containers, running or stopped, whose image is imageID.
func containersUsing(ctx context.Context, imageID string) ([]string, error) {
	out, err := output(ctx, "docker", "ps", "-aq", "--no-trunc")
	if err != nil {
		return nil, fmt.Errorf("%s", firstLine(strings.TrimSpace(out)))
	}
	ids := strings.Fields(out)
	if len(ids) == 0 {
		return nil, nil
	}
	args := append([]string{"inspect", "-f", "{{.Image}} {{.Name}}"}, ids...)
	out, err = output(ctx, "docker", args...)
	if err != nil {
		return nil, fmt.Errorf("%s", firstLine(strings.TrimSpace(out)))
	}
	var users []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if img, name, ok := strings.Cut(strings.TrimSpace(line), " "); ok && img == imageID {
			users = append(users, strings.TrimPrefix(name, "/"))
		}
	}
	return users, nil
}

// FreeBytes is the free space on the filesystem(s) holding Docker's images, from the HOST's point
// of view. Belay usually runs in a container that cannot see them, so a helper container mounts
// /var/lib and asks df. With the containerd image store the layers live in /var/lib/containerd and
// containers in /var/lib/docker; when those are different filesystems, the smaller one is the limit.
func FreeBytes(ctx context.Context) (int64, error) {
	const script = `for d in /h/containerd /h/docker /r; do [ -d "$d" ] && df -Pk "$d" | tail -n 1; done; true`
	args := []string{"-v", "/var/lib:/h:ro"}
	if root := dockerRoot(ctx); root != "" && !strings.HasPrefix(root, "/var/lib/") {
		args = append(args, "-v", root+":/r:ro") // a relocated data-root lives elsewhere
	}
	args = append(args, helperImage, "sh", "-c", script)
	out, err := runHelper(ctx, "df", args...)
	if err != nil {
		return 0, fmt.Errorf("measure free space: %s", firstLine(strings.TrimSpace(out)))
	}
	return parseDFAvail(out)
}

// parseDFAvail takes the smallest "Available" column (KiB) from `df -Pk` data lines.
func parseDFAvail(out string) (int64, error) {
	var free int64 = -1
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		kb, err := strconv.ParseInt(f[3], 10, 64)
		if err != nil {
			continue
		}
		if b := kb * 1024; free < 0 || b < free {
			free = b
		}
	}
	if free < 0 {
		return 0, fmt.Errorf("measure free space: no usable df output")
	}
	return free, nil
}

// dockerRoot is the daemon's data-root, when the API will say (a socket-proxy with INFO=0 won't).
func dockerRoot(ctx context.Context) string {
	out, err := output(ctx, "docker", "info", "--format", "{{.DockerRootDir}}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// PullNeed estimates the disk space pulling an image takes, from its compressed layer size.
//
// The containerd image store keeps both the compressed blobs it downloaded and the unpacked layers,
// so a pull costs compressed + uncompressed. Measured on galaxy (2026-10-22): authentik's 378 MB
// download occupied 0.38 + 1.94 GB = 6.1x; prometheus's 114 MB took 0.11 + 0.38 GB = 4.3x. 7x
// covers the worst of those with room, and the flat 1 GiB keeps a small image from being let in
// with the filesystem left at a few megabytes. It overestimates when layers are already present,
// which only matters on a disk that is nearly full — exactly when a warning is worth having.
func PullNeed(compressed int64) int64 {
	return 7*compressed + 1<<30
}

// checkSpace refuses a pull the image store cannot hold. Not being able to measure is not a reason
// to block an update, so that case passes.
func checkSpace(ctx context.Context, r engine.Request) error {
	if r.PullBytes <= 0 {
		return nil
	}
	free, err := FreeBytes(ctx)
	if err != nil {
		return nil
	}
	if need := PullNeed(r.PullBytes); free < need {
		return fmt.Errorf("not enough disk space to pull %s: it downloads %s, which needs about %s "+
			"on the image store once unpacked, and only %s is free. Nothing was changed; free some "+
			"space (unused images: docker image ls) and retry",
			r.ToImage, HumanBytes(r.PullBytes), HumanBytes(need), HumanBytes(free))
	}
	return nil
}

// HumanBytes formats a byte count the way df -h would, in decimal units like Docker's own output.
func HumanBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}

func shortID(id string) string {
	if _, hex, ok := strings.Cut(id, ":"); ok && len(hex) > 12 {
		return hex[:12]
	}
	return id
}

func firstLine(s string) string {
	return strings.TrimSpace(strings.SplitN(s, "\n", 2)[0])
}
