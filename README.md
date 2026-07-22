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

## Architecture

One binary, two roles:

- **`belay server`** — the web UI + brain + a built-in agent for its own host. Single-host users
  run only this.
- **`belay agent`** — a headless executor for other hosts. It **dials out** to the server over a
  token-authenticated WebSocket (works through NAT/firewalls, no inbound ports; TLS by default,
  skippable on trusted networks like a WireGuard tunnel).

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
