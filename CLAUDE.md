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
     and RCON port (25575, bound to `127.0.0.1` only) to random host ports, and generates a random
     RCON password per instance.
   - `WaitReady` polls RCON (`list` command) until it succeeds, while a separate faster ticker polls
     container state via inspect to fail fast on crash/OOM (with recent logs attached to the error).
     There's also an unused alternative `WaitReady_Logger` that follows container logs for a
     "Done (" marker instead — kept as a fallback strategy, not currently called anywhere.
   - `Stop` stops and optionally removes (with volumes) the container.
   - Host/container port mapping and the generated `RCONPassword` end up on the returned `*Instance`,
     which is the handle callers use for everything downstream (server connection info, RCON, stop).

2. **`rcon.go`** — Low-level `RCON` interface (`Exec`, `Close`, `Reconnect`) implemented over
   `github.com/gorcon/rcon`. Since gorcon's API isn't context-aware, dial and exec both run in a
   goroutine racing a `context.WithTimeout` (10s each). `DialRCON` is a standalone entry point for
   callers that already have a running server (outside this module's own `Manager`) and just want an
   `RCONHelper` — e.g. it's the integration point consumers like `mc-agent` use directly.

3. **`rcon_helpers.go`** — `RCONHelper` wraps a raw `RCON` with vanilla-command builders (`SetTime`,
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

4. **`rcon_block.go`** — `BlockResolver` is a small helper for probing what block occupies a
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
