# 🧠 hivemind

> Deploy and supervise a fleet of [Claude Code](https://docs.claude.com/en/docs/claude-code) agents from a single static binary.

**hivemind** turns Claude Code into a team. Each agent is a persistent, role-bound
Claude Code session with its own workspace and tools. A deterministic control plane
tracks every agent's state, tokens, and output — with **zero token cost for tracking** —
and a live terminal dashboard lets you prompt agents, route work, read their full
responses, and grant permissions on the fly.

One binary. One command. No idle agents burning tokens.

---

## Why

- **Persistent agents, ephemeral processes.** An agent is a named Claude Code session
  (pinned `--session-id`) bound to a workspace and a role. The session lives on disk;
  the *process* is ephemeral — each prompt spawns `claude -p --resume <id>`, runs to
  completion, and exits. Nothing idles burning tokens.
- **A deterministic control plane.** All mechanical tracking — liveness, activity,
  token totals, tool health — is derived from transcripts and locks with **no LLM
  involved**. It's exact and free.
- **A real supervisor.** A dedicated supervisor session (a small, cheap model)
  orchestrates by calling the same `hivemind` commands you do: it summarizes the
  fleet and routes work to the right agents — it never does worker work itself.
- **A live, Claude-Code-style console.** One TUI to prompt agents, read their full
  output, manage tools, delegate tasks, grant permissions, and interrupt runaway turns.

---

## Features

- **One unified command.** Run `hivemind` in a directory — it sets up a fleet if there
  isn't one, auto-starts everything if there is, and opens the console. Quit leaves the
  fleet running; reopen to reconnect.
- **Claude-Code-style console** with a welcome banner, a `❯` prompt, a slash-command
  palette that filters as you type (`/`), command history, and an animated working
  spinner with elapsed time.
- **Read an agent's full message** — press `enter`/`v` on any agent to open a detail
  overlay with its complete, word-wrapped output (no table truncation).
- **Grant permissions from the dashboard** — when an agent is blocked needing a
  capability (e.g. `WebFetch`), a numbered prompt lets you grant it and resume the
  agent in one keystroke. Arbitrary rules can be granted explicitly with `/grant`.
- **Interrupt a running turn** — press `esc` to stop an in-flight turn (kills the
  underlying process group), or `hivemind interrupt <agent>` from the CLI.
- **First-class tools** — `service` (long-running, health-checked, shown with uptime
  and port), `command` (an ad-hoc CLI the agent invokes), and `library` (a file the
  agent reads). Attach tools at setup or drop them in later.
- **Task delegation** — `hivemind delegate "ask api to refresh the cache and docs to
  rebuild the index"` and the supervisor decomposes it into per-agent tasks, tracked
  on a board and auto-completed when each agent's turn finishes.
- **Exact token tracking** — input/output token totals per agent, derived from
  transcripts (no estimation).
- **Read-only path enforcement** — `reads:` grants become Claude `permissions.deny`
  rules so an agent can read but not modify another agent's outputs.
- **Works without Claude installed** — a built-in fake runner (`--fake`) writes
  Claude-format transcripts so you can exercise the entire control plane and dashboard
  for testing or demos.

---

## Requirements

On the host that runs the fleet:

- **[Claude Code](https://docs.claude.com/en/docs/claude-code)** (`claude` on `PATH`,
  or set `HIVEMIND_CLAUDE_BIN`)
- **tmux** (used to run and supervise service tools)
- **Go ≥ 1.22** to build from source

`docker` is optional (only if your configs use it).

---

## Install

```sh
# from source — produces a single static binary
git clone https://github.com/spyderweb47/hivemind.git
cd hivemind
go build -o hivemind .
sudo mv hivemind /usr/local/bin/

# …or use the installer (detects OS/arch, checks deps, builds from source)
./install.sh            # --systemd also installs a boot unit
```

> No Claude installed yet? You can still run the whole thing with `hivemind --fake`,
> which uses a fake runner that writes Claude-format transcripts.

---

## Quickstart

```sh
hivemind        # that's the whole interface
```

Run it in a directory and it figures out what to do:

- **No fleet here yet?** → the setup wizard, then straight into the console.
- **Fleet already configured?** → it auto-starts service tools and background daemons,
  then opens the console.
- **Quit (`q`)** leaves the fleet running in the background. Run `hivemind` again to
  reconnect; everything is restored from on-disk state.

The individual verbs (`setup`, `up`, `down`, `clean`, `reset`, …) still exist for
scripting, but you never need them interactively.

---

## The console

The console shows your agents, service tools, a recent-events feed, and a command bar.

**Prompt and read**

- **Type plain text** → sent to the selected agent (`↑/↓` to pick the agent).
- **`enter` / `v`** on an agent → open the **detail overlay** with its full,
  untruncated, word-wrapped last message. If the agent is **blocked**, the overlay
  shows a numbered permission prompt.
- **`esc`** while a turn is running → **interrupt** it.

**Slash commands** (type `/` to open the filtering palette)

| Command | What it does |
|---|---|
| `/delegate <instruction>` | Route work across agents (the supervisor decomposes it). |
| `/task <agent> <prompt>` | Create one task for a specific agent. |
| `/send <agent> <prompt>` | Prompt a specific agent. |
| `/grant <agent> <rule…>` | Grant a permission rule (e.g. `WebFetch`) and resume. |
| `/attach <agent>` | Drop into the agent's live interactive session. |
| `/tool start\|stop\|restart <name>` | Manage a service tool. |
| `/up` · `/stop` · `/report` | Start / stop the fleet · wake the supervisor for a digest. |
| `/reset` | Force-stop and reset all sessions, restart fresh (also the `X` key). |
| `/destroy` | Delete the whole project (back to setup). |
| `/refresh` · `/help` · `/quit` | Console controls (quit leaves the fleet running). |

Aliases work too (`/new` → reset, `/q` → quit, `/?` → help, …).

**Keys:** `tab` switch focus · `↑/↓` select agent · `enter`/`v` view full message ·
`esc` interrupt · `t` attach a tool · `X` reset · `?` help · `q` quit.

---

## Concepts

### Agent

`{ name, workspace, model, role, tools[], reads[], session_id }`. At setup, hivemind
creates the workspace, attaches tools, writes the agent's `.claude/CLAUDE.md` (role +
each tool's docs + workspace confinement + a reporting contract) and
`.claude/settings.json` (permission rules + a `Stop` hook), and assigns a session id.

### Tool

A registered script/file plus instructions, attached to one or more agents:

```
tools/<name>/
  tool.yaml      # manifest (name, type, entrypoint, health, ports, restart)
  TOOL.md        # how the agent should use it — appended to its CLAUDE.md
  <artifact>     # the script/file
```

- **service** — long-running. Started in its own tmux window, health-probed on an
  interval, shown as `RUNNING` / `UNHEALTHY` / `STOPPED` with uptime and port. With
  `restart: on-failure`, a health daemon auto-restarts it.
- **command** — an ad-hoc CLI the agent invokes; `TOOL.md` tells it how.
- **library** — a foundation file the agent reads; no process.

You can register tools after setup from the CLI (`hivemind tool add …`) or from the
dashboard (select an agent, press `t`).

### Supervisor

A fixed agent (`name: supervisor`) whose CLAUDE.md teaches it the hivemind CLI. Its job:
read worker transcripts and tool health, summarize for you (into `ledger.md`, on each
turn and on a heartbeat), and route work — by calling the same commands you do.

### Delegation & tasks

A **task** is one unit of work routed to one agent, tracked on a board
(`.hivemind/tasks.json`) with a status (`pending → dispatched → done/blocked/failed`).

```sh
hivemind delegate "ask api to refresh the cache and docs to rebuild the index"
hivemind task api "run the migration and report row counts"
hivemind tasks            # the board + statuses
```

The supervisor decomposes a high-level instruction into per-agent prompts (if it names
an agent, that agent gets it; otherwise the supervisor picks by role). Tasks
auto-complete when the assigned agent's turn finishes, via its `Stop` hook.

### Permissions & grants

Each agent's allowed actions are encoded in its `.claude/settings.json`. When an agent
hits a wall it ends its turn with `BLOCKED:` and what it needs; hivemind flips it to
`BLOCKED`. You then grant the capability — from the dashboard's permission prompt or
with:

```sh
hivemind grant api WebFetch WebSearch       # adds allow rules, regenerates settings, resumes
```

Grants are appended to the agent's `permissions.allow` and recorded in
`.hivemind/grants.log`. The dashboard's one-keystroke grant is limited to safe built-in
Claude tools; anything else (e.g. a `Bash(...)` rule) must be granted explicitly with
`/grant`, so it's always a deliberate, user-authored decision.

---

## CLI reference

| Command | Purpose |
|---|---|
| `hivemind` | Auto-detect: set up if needed, start the fleet, open the console. |
| `hivemind setup [--preset f]` | Interactive onboarding wizard (or a preset file for CI). |
| `hivemind up` / `down` | Start/stop service tools (tmux) + health/heartbeat daemons. |
| `hivemind status [--json]` | Agents (state, activity, task, tokens) + tools (status, port). |
| `hivemind send <agent> "…"` | Prompt an agent (headless resume; non-blocking; lock-guarded). |
| `hivemind interrupt <agent>` | Stop an agent's in-flight turn. |
| `hivemind grant <agent> <rule…>` | Grant permission rule(s) and resume the agent. |
| `hivemind delegate "…"` | Route a high-level instruction to the right agents. |
| `hivemind task <agent> "…"` | Create + dispatch one explicit task. |
| `hivemind tasks [--json] [--open]` | Show the task board. |
| `hivemind attach <agent>` | Drop into the agent's live session (`claude --resume`). |
| `hivemind transcript <agent> [--tail N]` | Pretty-print the transcript tail. |
| `hivemind tool start\|stop\|restart\|status <tool>` | Manage a service tool. |
| `hivemind tool add <name> …` / `tool attach <tool> <agent>` | Register/attach a tool after setup. |
| `hivemind add agent\|tool <name> …` | Add an agent or tool after setup. |
| `hivemind edit <agent> [--model …]` | Modify an agent (model/role/tools/reads) + re-scaffold. |
| `hivemind report` | Wake the supervisor to summarize the fleet → `ledger.md`. |
| `hivemind events [agent]` | Show the per-turn push feed (`Stop`-hook events). |
| `hivemind logs <agent\|tool>` | Tail an agent's runner log or a tool's tmux pane. |
| `hivemind clean [--purge] [--yes]` | Stop everything + free disk; `--purge` also removes workspaces. |
| `hivemind reset [--yes]` | Force-stop, reset all sessions, restart fresh. |

Global flags: `--root <dir>` (project root; default = nearest `.hivemind` or cwd),
`--fake` (use the fake runner).

---

## Configuration

`hivemind setup` writes `.hivemind/config.yaml`. You can also supply it directly
(or via a preset for non-interactive setup):

```yaml
project: example
supervisor:
  model: haiku
  report: { on_event: true, heartbeat_minutes: 30 }
defaults:
  model: sonnet
  permission_mode: acceptEdits
tools:
  - { name: scraper,  type: service, entrypoint: "python scraper.py", health: "curl -sf localhost:9000/health", ports: [9000] }
  - { name: template, type: library, path: template.txt }
agents:
  - { name: api,      workspace: api,      model: sonnet, tools: [scraper],  role: "Own the API service and its data." }
  - { name: research, workspace: research, model: opus,   reads: [api/data], role: "Analyze outputs; never modify api/data." }
```

A preset is this schema plus optional `tool_sources` (files to drop in) and `tool_docs`
(TOOL.md content) maps.

---

## On-disk layout

```
<project-root>/
  .hivemind/
    config.yaml              source of truth (from setup)
    agents/<name>/           session_id, lock, events.log, runner.log, status cache
    tools/<name>/            service-tool runtime state (pid, started-at)
    events.log               Stop-hook events (all agents)
    ledger.md                supervisor digest
    grants.log               audit trail of permission grants
    supervisor/              the supervisor's workspace
  tools/<name>/              registered tools (tool.yaml, TOOL.md, artifact)
  <workspace>/               one per agent (its cwd); contains .claude/{CLAUDE.md,settings.json}
```

Transcripts are written by Claude Code under `~/.claude/projects/<…>/<session-id>.jsonl`.
hivemind never hardcodes that path — it locates a transcript by globbing for the agent's
pre-assigned `<session-id>.jsonl`.

---

## How liveness & tokens are derived (no LLM)

- **State** comes from the per-agent lock + the transcript: `WORKING` if a turn is in
  flight (lock held) or the transcript advanced within ~10s; `ERROR` if the last turn
  errored; `BLOCKED` if a turn flagged it needs input; `IDLE` otherwise; `NEW` if the
  session hasn't started.
- **Tokens** are summed exactly from the transcript (input/output, including cache).

## Isolation

`reads:` grants are enforced at the tool layer: setup generates Claude
`permissions.deny` rules for the file-mutating tools on those paths, so Claude refuses
to write/edit them while still allowing reads. Workspace edits stay auto-accepted.

> The permission mode is carried by `settings.json`, **not** the `--permission-mode`
> CLI flag — passing that flag would bypass `deny` rules.

This is a real guardrail but **not a full sandbox**: a determined agent could still
write via `Bash`. OS-level isolation (containers, read-only mounts) is on the roadmap.

---

## Development

```sh
go build -o hivemind .
go vet ./...
go test ./...
```

The fake runner (`--fake` or `HIVEMIND_FAKE_RUNNER=1`) writes Claude-format transcripts,
so the entire control plane, dashboard, and tests run without the real `claude` CLI.

---

## License

[MIT](./LICENSE)
