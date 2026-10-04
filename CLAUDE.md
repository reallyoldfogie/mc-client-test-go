# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A Go module (`github.com/reallyoldfogie/mc-client-test-go`) for spinning up disposable Minecraft
server containers via Docker (`itzg/minecraft-server` image) and driving them over RCON, for
automated client-compatibility testing. It's a library intended to be imported from other test
suites (e.g. `go test` integration tests, or consumers like `mc-agent`), not a standalone service.

## Commands

```bash
go build ./...                 # build
go vet ./...                   # vet
go test ./...                  # unit tests (no Docker required for pure package tests)

# Run the Docker/RCON integration test (requires a running Docker daemon):
go test ./... -run TestClientCompatibility -v
```

Note: `integration_test.go`'s build tag comment is `// Xgo:build integration` (not a real
`//go:build` constraint), so it currently compiles and runs as part of the normal `go test ./...`
invocation rather than being gated behind a build tag — it will attempt to talk to Docker whenever
tests run. If you add a real build tag back, update the run command in the README accordingly.

There is no linter config or Makefile in this repo; `go vet` and `gofmt` are the available checks.

## Architecture

Everything lives in the `testenv` package (imported as `mcclienttest`'s dependency from
`integration_test.go` at the repo root). Three layers:

1. **`manager.go`** — `Manager` wraps a Docker client (`github.com/moby/moby/client`) and owns the
   container lifecycle for a test server:
   - `Start` creates a container from `ServerConfig` (version, image, env, optional bind mounts for
     data/mods/config/output/arbitrary dirs), publishes the Minecraft port (25565, all interfaces)
     and RCON port (25575, bound to `127.0.0.1` only *unless* talking to a genuinely remote daemon
     over plain `tcp://` with no SSH tunnel involved — `resolveDaemonHost` is what tells that apart
     from a local daemon or an SSH-tunneled one, both of which keep RCON on loopback), and generates
     a random RCON password per instance.
   - `resolveDaemonHost(cli)` inspects the Docker client's actual resolved host (`cli.DaemonHost()`)
     to classify it as local (`unix://`/`npipe://`, or a `tcp://`/`ssh://` host that's itself
     `localhost`/`127.0.0.1`/`::1`) or genuinely remote — used for both the RCON bind-IP decision
     above and for rejecting local directory binds against a daemon with no access to this
     machine's filesystem (a direct remote `tcp://` daemon only; SSH-tunnel mode handles binds via
     `ssh_sync.go` instead of rejecting them — see below).
   - If `m.sshTarget` is set (populated by `NewManager` when `DOCKER_HOST` is `ssh://...` — see
     `ssh_tunnel.go`), `Start` opens a dedicated tunnel for this one container via
     `startSSHTunnel`/`syncLocalBindsToRemote` *before* doing anything else, and uses that tunnel's
     own client (`tunnel.cli`) for every Docker API call in the rest of the function instead of
     `m.cli` — tracked per-container in `m.tunnels` (keyed by container ID) so later calls
     (`WaitReady`, `Logs`, `Stop`) can look up the right client via `clientForContainer`.
   - `WaitReady` polls RCON (`list` command) until it succeeds, while a separate faster ticker polls
     container state via inspect to fail fast on crash/OOM (with recent logs attached to the error).
     There's also an unused alternative `WaitReady_Logger` that follows container logs for a
     "Done (" marker instead — kept as a fallback strategy, not currently called anywhere.
   - `Stop` stops and optionally removes (with volumes) the container, and — if this container had
     an SSH tunnel — closes it and removes its remote-staged directory (if any) afterward.
   - Host/container port mapping and the generated `RCONPassword` end up on the returned `*Instance`,
     which is the handle callers use for everything downstream (server connection info, RCON, stop).
     `Instance.Host` is `127.0.0.1` for both local and SSH-tunneled daemons, and the real remote
     hostname only for a direct, un-tunneled `tcp://` remote daemon.

2. **`ssh_tunnel.go`** — SSH tunnel lifecycle for `DOCKER_HOST=ssh://...` mode. `startSSHTunnel`
   shells out to `ssh -N` with three `-L` forwards (the remote Docker socket, plus the container's
   game and RCON ports — the *same* port number used on both ends, so nothing needs to track two
   different numbers for "local" vs. "remote"), then polls `Ping` on a client built against the
   forwarded socket until it succeeds, the process exits, or a timeout elapses. Lifecycle
   correctness detail worth knowing before touching this: `processExited` checks a `waitDone`
   channel closed by a dedicated background `cmd.Wait()` goroutine started right after `cmd.Start()`
   — **not** `cmd.Process.Signal(0)`, which was tried first and found to be unreliable: a process
   that's exited but not yet reaped (a zombie) still answers signal-0 as "alive," so a dead tunnel
   would never be detected as dead that way. `cmd.Wait()` must only ever be called once per
   process; `Close()` doesn't call it again, it just waits on the same channel.

3. **`ssh_sync.go`** — stages local directory binds (`DataDir`/`ModsDir`/`ConfigDir`/`OutputDir`/
   `MountDirs`) to the remote host for SSH-tunnel mode, since a remote daemon has no access to this
   machine's filesystem. `planLocalBinds` is the pure half (which local dir goes under which remote
   subdirectory, bound to which container path) — deliberately separated from `syncDirToRemote`
   (which actually transfers, via `tar -C localDir -cf - . | ssh target tar -C remoteDir -xf -`) so
   the destination-naming logic is unit-testable without any live SSH involved. `MountDirs` needed
   special handling: a local-daemon bind uses each entry as *both* the source path and (via
   `/data/<value>`) the destination name, which breaks once the source is rehomed under a staging
   root — the destination name is taken from `filepath.Base` of the original value instead.
   `removeRemoteDir` cleans up a container's staging directory from `Stop`.

4. **`rcon.go`** — Low-level `RCON` interface (`Exec`, `Close`, `Reconnect`) implemented over
   `github.com/gorcon/rcon`. Since gorcon's API isn't context-aware, dial and exec both run in a
   goroutine racing a `context.WithTimeout` (10s each). `DialRCON` is a standalone entry point for
   callers that already have a running server (outside this module's own `Manager`) and just want an
   `RCONHelper` — e.g. it's the integration point consumers like `mc-agent` use directly.

5. **`rcon_helpers.go`** — `RCONHelper` wraps a raw `RCON` with vanilla-command builders (`SetTime`,
   `SetWeather`, `SetGamerule`, `Teleport`, `SetBlock`, `SummonEntity`, entity data/position getters,
   etc). Two things worth knowing before touching this file:
   - Most builder methods (`SetTime`, `SetGamerule`, ...) do **not** execute immediately — they
     return an `*RCONResult` (a deferred command). Call `.Exec(ctx)` on it, or pass several into
     `ExecuteMany(ctx, ...*RCONResult)` to run them in sequence and collect responses keyed by
     command string. `ExecuteMany` returns on the first error and does not roll back prior commands.
     `RCONResult.Exec` runs through `ExecuteWithRetry` (3 attempts, reconnecting on `EOF` errors).
   - `NewRCONHelper` auto-detects the server's Minecraft version by running `version` over RCON at
     construction time (best-effort; failure just leaves version detection off). `SetGamerule` uses
     that detected version to pick pre-/post-1.21.11 gamerule syntax (old camelCase names vs. new
     `minecraft:snake_case` namespaced names) — if you add new gamerules, extend `newNameMap` rather
     than assuming one syntax works across versions.

6. **`rcon_block.go`** — `BlockResolver` is a small helper for probing what block occupies a
   coordinate, since Minecraft has no direct "get block type" RCON command: it iterates a candidate
   block ID list and runs `execute if block x y z <id> run say <id>` until one matches, caching
   per-coordinate results (`"x,y,z"` → id, with `""` meaning "no match found").

## Working with this codebase

- Any change to `RCON`/`RCONHelper` method signatures affects external consumers (this module is
  imported elsewhere, e.g. `mc-agent`) — check call sites in the README's usage examples stay
  accurate, since the README is the primary API documentation surface here.
- RCON command strings sent to the server are not escaped/sanitized beyond basic whitespace
  trimming (`Say`); when adding new helpers that interpolate untrusted input into a command string,
  be mindful this is effectively command construction against the server console.
- The SSH tunnel/sync code (`ssh_tunnel.go`, `ssh_sync.go`) has no sandbox-safe way to test the
  actual `ssh`/`tar` subprocess execution against a real remote host — there's typically no local
  `sshd` with key-based auth to itself available. What *is* unit-tested: URL/host parsing
  (`parseSSHDockerHost`, `resolveDaemonHost`), free-port allocation, process lifecycle
  (`processExited`/`Close`, including the zombie-detection fix described above), and the pure
  bind-planning logic (`planLocalBinds`) — each deliberately factored out from the actual
  subprocess calls specifically so it *could* be tested without live infrastructure. Changes to the
  actual transfer/tunnel mechanics need a real remote host to verify by hand.
