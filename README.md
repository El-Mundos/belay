# ⚓ Belay

**Deploy without fear — Belay catches the fall.**

Belay is a lightweight, self-hosted tool for **safely updating Docker Compose stacks**. It checks
for new image versions, shows you the changelogs, updates on your say-so, and — the whole point —
**automatically rolls back any update that fails to come up healthy**, handing you the logs so you
can fix it and retry. One web UI, across one host or many.

> The name: in climbing, the *belayer* is the person who catches you if you fall. That's exactly
> what this does for your container updates.

## Why it exists

The Docker-update landscape has a hole in the middle:

| | Detect | Apply | Safe (health-gated rollback) | UI |
|---|:---:|:---:|:---:|:---:|
| **Checkers** (Diun, WUD, Cup) | ✅ | ❌ | ❌ | some |
| **Appliers** (Watchtower) | ✅ | ✅ | ❌ *(reckless)* | ❌ |
| **Platforms** (Portainer, Komodo) | ~ | ✅ | ❌ *(manual rollback)* | ✅ |
| **Belay** | ✅ | ✅ | **✅ automatic** | ✅ |

Nobody does **lightweight + *safe* apply**: update with a health gate and automatic rollback.
That's Belay's one sharp differentiator. *"Watchtower, but it won't leave you with a broken container."*

## The core: the safe-update loop

```
for each selected service:
  record current image tag
  rewrite tag in the compose file        (edit-in-place; git commit if repo)
  docker compose up -d <service>
  run the HEALTH-GATE LADDER (with timeout):
      1. Docker HEALTHCHECK, if the image defines one
      2. else a configured probe (HTTP 200 / TCP open / log-line match)
      3. else "stayed running for N seconds"
  healthy?  → keep it, record success
  NOT healthy? → capture logs, revert the tag, `up -d` the old image, report ROLLED_BACK
```

Rollback scope is **image-tag only** (clean and reliable). Config-coupled updates (a new version
needing a new env var) revert the tag and tell you to fix the config and retry — by design.

### Tracking every kind of tag

Container tags move in two different ways, and only tracking one of them leaves services silently
stuck, so Belay watches both:

- **The tag name changes** — `1.27.1` → `1.27.2`. Compared by version and applied by rewriting the
  tag in your compose file. Tags are ordered purely by version, ignoring how many components they
  have, so a service climbs to whatever is genuinely newest: `15` → `15.4.0` → `16` → `16.2.0`.
  Where a registry spells one release several ways (`15.4` and `15.4.0`), the most precise wins —
  the vaguer form would drift to a different build later. A registry mixing schemes (a calver
  `20260819` next to a semver `1.2.3`) will rank the calver higher, because numerically it is; pin
  the service if that isn't what you want.
- **The tag stays, the build behind it changes** — a `latest` / `stable`, or a rebuild published
  under a tag you already run. There is no new name to notice, so Belay compares the registry's
  digest for the tag against the digest you actually pulled. Applying it re-pulls the same tag and
  leaves your compose file untouched. This is the only signal for tags no version scheme can order.

Because a rebase keeps the tag, "roll back to the old tag" would mean pulling the build that just
failed. Belay instead pins the rollback to the digest that was running, so a failed rebase leaves the
service on the exact image it had before — and stops the bad build being pulled again.

> Digest tracking inspects the local image, so it currently covers services on the Belay host.
> Remote agents report their images but not yet their digests, so rebases there go unnoticed.

### What the health gate cannot catch

Being honest about the edges, because a safety tool that overstates its guarantees is worse than one
that doesn't:

- **"Healthy" is not "correct."** The gate proves a container started and answers; it cannot prove
  the new version behaves. A service can pass every rung while being subtly broken — an identity
  provider that boots fine but silently drops a claim from its tokens, say.
- **A rollback can be incomplete.** Reverting the tag restores the *code*, not everything the failed
  version did on its way down. The common case is a new version writing schema migrations to a
  database owned by a **sibling service** in the same stack: belay's volume snapshot covers the
  service being updated, so it never took that sibling's data. Belay now flags this when it happens,
  but flagging is all it can do — undoing it is yours.
- **Therefore: group services that must move together.** If a version split between two services
  breaks them (an app and its migration worker), put them in the same group in Settings so a partial
  application is reverted rather than left half-done.

## Architecture

One binary, two roles:

- **`belay server`** — the web UI + brain + a built-in agent for its own host. Single-host users
  run only this.
- **`belay agent`** — a headless executor for other hosts. It **dials out** to the server over a
  token-authenticated WebSocket (works through NAT/firewalls, no inbound ports; TLS by default,
  skippable on trusted networks like a WireGuard tunnel).

## Quick start

```bash
docker build -t belay .

# auto-discovers your running compose stacks via the Docker socket — no config needed
docker run -d --name belay \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v /srv:/srv \                       # your compose files, so it can edit + git-commit them
  -p 127.0.0.1:8080:8080 \
  belay server --addr 0.0.0.0:8080
# then open http://127.0.0.1:8080
```

Or the CLI:

```bash
belay check  ./mystack                              # what's outdated?
belay update ./mystack grafana grafana/grafana-oss:13.1.0   # safe update + auto-rollback
```

Gate it behind your reverse-proxy SSO with `--forward-header X-authentik-username`, or use the
built-in login with `--password` / `BELAY_PASSWORD`.

## Design decisions (locked)

- **Language:** Go — single static binary, tiny image.
- **UI:** server-rendered HTML + htmx + SSE (live logs/status). No JS framework, no build step.
- **Auth:** built-in login **+** optional forward-auth header (so it can sit behind SSO like Grafana).
- **Update model:** edit the tag in your compose file in place; if it's a git repo, commit each
  change (free history + `git revert` rollback).
- **Multi-host:** agents dial out to the server (WebSocket, token, TLS-optional).
- **License:** AGPL-3.0 (+ DCO for contributions).

## Non-goals (the discipline that keeps it lightweight)

- ❌ Not a Kubernetes tool. Compose only.
- ❌ Not auto-rollback for *config-coupled* updates — tag rollback only.
- ❌ Not a full PaaS (build pipelines, git-deploy). It updates what you already run.
- ❌ Not another registry/vuln-scanner/SBOM kitchen sink.

## Status

🚧 Early scaffold. Building the engine first (health-gated deploy + rollback), then the agent,
then the server + UI.

## License

[AGPL-3.0](./LICENSE).
