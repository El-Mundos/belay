package server

import (
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"

	"github.com/El-Mundos/belay/internal/store"
)

// recView is one stored attempt prepared for display. History and Failed render the same card
// shape, so the preparation lives here instead of being duplicated (and drifting) in each handler.
//
// Logs and the full error text are deliberately NOT part of this view: both lists re-render on a
// poll, and carrying kilobytes of container output through every refresh is what made them heavy
// and jumpy. The details popout fetches them per record from /record instead.
type recView struct {
	store.Record

	// Repo is the image name when both sides of the update share it, in which case FromRef/ToRef
	// are bare tags. Showing "prom/prometheus:v3.13.2 → prom/prometheus:v3.14.0" repeats the one
	// part that cannot differ — an update never changes which image a service runs.
	Repo    string
	FromRef string
	ToRef   string

	ErrPreview string // first line of Err, clipped — see errPreview
	ErrMore    bool   // the preview dropped something, so offer the popout
	HasDetails bool   // there are logs or an error worth opening

	// History-only: state of the manual-rollback button.
	CanRollback  bool   // newest success for this service, still within the retention window
	SelfRollback bool   // this is Belay's own update and the previous image is still retained
	Superseded   bool   // an older success whose rollback point was replaced by a newer update
	PID          int    // project id, for the rollback POST
	Expires      string // human "expires …" for the button tooltip

	// RollbackWhy explains a DISABLED button. Empty means show no button at all — a row that never
	// had a rollback point must not be dressed up as one whose window expired, which is what a
	// blanket "expired" tooltip did to Belay's own self-update rows.
	RollbackWhy string
}

// SelfUpdateProject / SelfUpdateService label Belay's own update in the shared History. They are
// constants because two places must agree on them: the writer (reconcileSelfUpdate) and the reader
// (the History handler, deciding whether to offer a self-rollback).
const (
	SelfUpdateProject = "belay"
	SelfUpdateService = "belay (self-update)"
)

// IsSelfUpdate reports whether a record is Belay updating itself rather than a compose service.
func (v recView) IsSelfUpdate() bool {
	return v.Project == SelfUpdateProject && v.Service == SelfUpdateService
}

func newRecView(r store.Record) recView {
	v := recView{
		Record:     r,
		FromRef:    r.From,
		ToRef:      r.To,
		HasDetails: r.Logs != "" || strings.TrimSpace(r.Err) != "",
	}
	if fr, ft := splitRef(r.From); fr != "" && ft != "" {
		if tr, tt := splitRef(r.To); fr == tr && tt != "" {
			v.Repo, v.FromRef, v.ToRef = fr, ft, tt
		}
	}
	v.ErrPreview, v.ErrMore = errPreview(r.Err)
	return v
}

func newRecViews(recs []store.Record) []recView {
	out := make([]recView, 0, len(recs))
	for _, r := range recs {
		out = append(out, newRecView(r))
	}
	return out
}

// splitRef splits an image reference into name and tag for display. A digest reference is split at
// the "@" first: its "sha256:…" would otherwise look like a tag separator.
func splitRef(image string) (repo, tag string) {
	if i := strings.LastIndex(image, "@"); i >= 0 {
		return image[:i], shortDigest(image[i+1:])
	}
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		return image[:i], image[i+1:]
	}
	return image, ""
}

// shortDigest clips sha256:<64 hex> to its first 12 hex characters. Nobody reads the other 52, and
// at full length it pushes everything else out of the row.
func shortDigest(d string) string {
	if algo, hex, ok := strings.Cut(d, ":"); ok && len(hex) > 12 {
		return algo + ":" + hex[:12]
	}
	return d
}

// errPreviewMax is where a preview line is cut. Long enough for a real Docker error to identify
// itself, short enough to stay one line on a normal window.
const errPreviewMax = 160

// errPreview reduces a stored error to something that fits on one line, and reports whether it
// dropped anything (so the card can offer the full text).
//
// The rule is "first non-empty line, clipped": a compose/daemon failure states what went wrong on
// its first line and then spends the rest of its length on a stack of context. Taking the tail
// instead would surface the outermost wrapper, which is nearly always the least specific part.
func errPreview(msg string) (preview string, more bool) {
	for _, line := range strings.Split(msg, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if preview == "" {
			preview = line
			continue
		}
		more = true
		break
	}
	if r := []rune(preview); len(r) > errPreviewMax {
		return strings.TrimRight(string(r[:errPreviewMax]), " ") + "…", true
	}
	return preview, more
}

// listRev fingerprints a list so the browser can tell an unchanged refresh from a real one and skip
// the DOM swap. Replacing identical markup every few seconds is what makes the page flash and drops
// whatever the user had selected mid-read.
func listRev(rows []recView) string {
	h := fnv.New64a()
	for _, r := range rows {
		fmt.Fprintf(h, "%d|%s|%s|%s|%s|%t|%t|%t|%s|%s;",
			r.ID, r.Outcome, r.From, r.To, r.Duration,
			r.CanRollback, r.SelfRollback, r.Superseded, r.Expires, r.RollbackWhy)
	}
	return strconv.FormatUint(h.Sum64(), 36)
}
