# mc-client-test-go

A small Go helper module for spinning up disposable Minecraft server instances in Docker
(using `itzg/minecraft-server`) and controlling them via RCON for automated client
compatibility testing.

Module path: `github.com/reallyoldfogie/mc-client-test-go`.

## Features

- Start a Minecraft server container for a specific version using Docker.
- Automatically map host ports to avoid conflicts.
- Wait until the server is ready before running tests.
- Expose connection info (host / port) for your Minecraft client.
- RCON client abstraction for sending commands.
- Convenience helpers for common RCON operations (time, weather, gamerules, teleport, etc.).
- Designed to be used from `go test` integration tests.

## Prerequisites

- Go 1.24+ (or adjust `go.mod` as needed).
- Docker daemon running locally and accessible via the standard environment
  (`DOCKER_HOST`, `/var/run/docker.sock`, etc.).
- Network + resources to run one or more Minecraft servers.

## Installation

```bash
git clone https://github.com/reallyoldfogie/mc-client-test-go.git
cd mc-client-test-go
go mod tidy
```

Or add it as a dependency to another module:

```bash
go get github.com/reallyoldfogie/mc-client-test-go@latest
```

> Note: until this is pushed to GitHub, `go get` will not work; use local replace
> directives in your own `go.mod` during development.

## Core Types

Package `testenv` exposes:

```go
type ServerConfig struct {
    Version    string            // e.g. "1.20.4", "1.21.3"
    Image      string            // default "itzg/minecraft-server:latest" if empty
    OnlineMode bool              // default false (offline mode)
    DataDir    string            // optional host path for /data (empty => ephemeral)
    ModsDir    string            // optional host path for /data/mods (empty => no mods)
    ConfigDir  string            // optional host path for /data/config (empty => no config)
    OutputDir  string            // optional host path for /data/output (empty => no output mount)
    ExtraEnv   map[string]string // extra env vars to pass through
    PullImage  bool              // if true, always pull image before start
    NamePrefix string            // container name prefix, e.g. "mc-test-"
    MountDirs  []string          // extra directories to mount, each to /data/<val>
}

type Instance struct {
    ID             string
    Name           string
    Version        string
    Host           string
    HostServerPort int
    HostRCONPort   int
    RCONPassword   string
}

type Manager interface {
    Start(ctx context.Context, cfg ServerConfig) (*Instance, error)
    WaitReady(ctx context.Context, inst *Instance) error
    Stop(ctx context.Context, inst *Instance, remove bool) error
    Logs(ctx context.Context, containerID string, logOptions client.ContainerLogsOptions) (client.ContainerLogsResult, error)
    RCONClient(ctx context.Context, inst *Instance) (RCON, error)
}

type RCON interface {
    Exec(ctx context.Context, cmd string) (string, error)
    Close() error
    Reconnect(ctx context.Context) error
}

// RCONHelper is an interface (not a struct) built on top of RCON that adds
// convenience methods for common commands. See "RCON Helper Functions" below.
type RCONHelper interface {
    RCON
    SetTime(ctx context.Context, spec string) *RCONResult
    SetWeather(ctx context.Context, weather string) *RCONResult
    SetGamerule(ctx context.Context, rule, value string) *RCONResult
    Teleport(ctx context.Context, target string, x, y, z float64) *RCONResult
    TeleportTo(ctx context.Context, target, destination string) *RCONResult
    Say(ctx context.Context, msg string) *RCONResult
    SetBlock(ctx context.Context, x, y, z int64, blockType, blockAction string) *RCONResult
    SummonEntity(ctx context.Context, x, y, z float64, entityType, nbtData string) *RCONResult

    GetEntityData(ctx context.Context, target, dataPath string) (parsedUser, data string, err error)
    GetEntityDimension(ctx context.Context, target string) string
    GetEntityPos(ctx context.Context, target string) (X, Y, Z float64, err error)

    ExecuteMany(ctx context.Context, cmds ...*RCONResult) (responses map[string]string, err error)
    ExecuteWithRetry(ctx context.Context, cmd string, maxRetries int) (string, error)
}
```

`DialRCON(ctx, host, port, password string) (RCONHelper, error)` connects directly to an
already-running server's RCON port (no `Manager`/container involved) and returns a ready-to-use
`RCONHelper` — useful when another tool already has a running server and just wants the helper
methods.

## Creating a Manager

```go
import "github.com/reallyoldfogie/mc-client-test-go/testenv"

mgr, err := testenv.NewManager()
if err != nil {
    // handle error
}
```

`NewManager` initializes a Docker client using `client.FromEnv` and API
version negotiation.

## Starting a Test Server

```go
ctx := context.Background()

cfg := testenv.ServerConfig{
    Version:    "1.21.3",
    PullImage:  true,          // pull image if not present
    OnlineMode: false,         // offline for tests
    NamePrefix: "mc-test-",    // optional
    ExtraEnv: map[string]string{
        "DIFFICULTY": "peaceful",
        "MODE":       "creative",
    },
}

inst, err := mgr.Start(ctx, cfg)
if err != nil {
    // handle error
}

// Wait for the server to be ready
if err := mgr.WaitReady(ctx, inst); err != nil {
    // handle error
}

// inst.Host and inst.HostServerPort can now be used by your Minecraft client
```

The module maps:

- Container `25565/tcp` (Minecraft) to a random available host port.
- Container `25575/tcp` (RCON) to a random available host port, bound to `127.0.0.1`.

`Instance.Host` is typically `127.0.0.1`.

## Stopping and Cleaning Up

```go
stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()

if err := mgr.Stop(stopCtx, inst, true); err != nil {
    // handle error
}
```

The `remove` flag controls whether the container (and volumes) are removed.

## Using RCON

```go
r, err := mgr.RCONClient(ctx, inst)
if err != nil {
    // handle error
}
defer r.Close()

reply, err := r.Exec(ctx, "list")
if err != nil {
    // handle error
}
fmt.Println("Server replied:", reply)
```

## RCON Helper Functions

For more convenience, wrap the raw `RCON` with an `RCONHelper`. `NewRCONHelper` also
best-effort auto-detects the server's Minecraft version (via the `version` command), which
`SetGamerule` uses to pick the right command syntax (see below).

Most helper methods (`SetTime`, `SetWeather`, `SetGamerule`, `Teleport`, `TeleportTo`, `Say`,
`SetBlock`, `SummonEntity`) build the command but **do not execute it immediately** — they return
an `*RCONResult`. Call `.Exec(ctx)` on it to run the command (with built-in retry/reconnect on
dropped connections), or batch several together with `ExecuteMany`:

```go
helper := testenv.NewRCONHelper(r)

// Run a single command immediately:
if _, err := helper.SetTime(ctx, "day").Exec(ctx); err != nil {
    // handle error
}

// Or batch several — ExecuteMany runs them in order and returns on the first error,
// with responses keyed by the command string that was sent:
responses, err := helper.ExecuteMany(ctx,
    helper.SetTime(ctx, "day"),
    helper.SetWeather(ctx, "clear"),
    helper.SetGamerule(ctx, "doDaylightCycle", "false"),
    helper.Teleport(ctx, "TestBot", 0, 80, 0),
)
if err != nil {
    // handle error
}
for cmd, resp := range responses {
    fmt.Println(cmd, "=>", resp)
}
```

Available helper methods:

- `SetTime(ctx, spec string)` → `time set <spec>` (e.g., `day`, `noon`, `1000`).
- `SetWeather(ctx, weather string)` → `weather <weather>` (e.g., `clear`, `rain`, `thunder`).
- `SetGamerule(ctx, rule, value string)` → `gamerule <rule> <value>`. Automatically translates
  camelCase rule names (e.g. `doDaylightCycle`) to the namespaced `minecraft:snake_case` syntax
  required on servers detected as version 1.21.11+; older/undetected versions use the original
  camelCase names as-is.
- `Teleport(ctx, target string, x, y, z float64)` → `tp <target> x y z`.
- `TeleportTo(ctx, target, destination string)` → `tp <target> <destination>`.
- `Say(ctx, msg string)` → `say <msg>`.
- `SetBlock(ctx, x, y, z int64, blockType, blockAction string)` → `setblock x y z <blockType> <blockAction>`
  (`blockAction` is one of `replace`, `destroy`, `keep`, `strict`).
- `SummonEntity(ctx, x, y, z float64, entityType, nbtData string)` → `summon <entityType> x y z <nbtData>`.
- `GetEntityData(ctx, target, dataPath string)` → runs `data get entity <target> <dataPath>` and
  parses the response into the matched entity name and raw data string.
- `GetEntityDimension(ctx, target string)` → the entity's current dimension (e.g. `minecraft:overworld`).
- `GetEntityPos(ctx, target string)` → the entity's `X, Y, Z` position as `float64`s.
- `ExecuteMany(ctx, cmds ...*RCONResult)` → executes multiple deferred commands in order, failing
  on the first error, returning responses keyed by command string.
- `ExecuteWithRetry(ctx, cmd string, maxRetries int)` → runs a raw command string, reconnecting and
  retrying on connection-dropped (`EOF`) errors. This is what `RCONResult.Exec` calls under the hood
  (with `maxRetries` = 3).

These are intentionally thin wrappers around the native Minecraft commands, so you
stay as close to vanilla behavior as possible while writing tests.

## Resolving Block Types (`BlockResolver`)

Minecraft has no direct "get block type at position" RCON command, so `BlockResolver` probes a
list of candidate block IDs against a coordinate using
`execute if block x y z <id> run say <id>`, returning the first match and caching results
per-coordinate:

```go
resolver := testenv.NewBlockResolver(helper, []string{
    "minecraft:stone",
    "minecraft:dirt",
    "minecraft:grass_block",
})

blockID, err := resolver.GetBlock(ctx, 0, 64, 0)
if err != nil {
    // no candidate matched
}
```

## Example Integration Test

See `integration_test.go` in this repo for a full example. In short:

```go
//go:build integration

package mcclienttest

import (
    "context"
    "testing"
    "time"

    "github.com/reallyoldfogie/mc-client-test-go/testenv"
)

func TestClientCompatibility(t *testing.T) {
    versions := []string{"1.20.4", "1.21"}

    mgr, err := testenv.NewManager()
    if err != nil {
        t.Fatalf("new manager: %v", err)
    }

    for _, v := range versions {
        v := v
        t.Run("version_"+v, func(t *testing.T) {
            t.Parallel()

            ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
            defer cancel()

            cfg := testenv.ServerConfig{
                Version:    v,
                PullImage:  true,
                OnlineMode: false,
                NamePrefix: "mc-test-",
            }

            inst, err := mgr.Start(ctx, cfg)
            if err != nil {
                t.Fatalf("start server: %v", err)
            }
            defer func() {
                stopCtx, cancelStop := context.WithTimeout(context.Background(), 30*time.Second)
                defer cancelStop()
                _ = mgr.Stop(stopCtx, inst, true)
            }()

            if err := mgr.WaitReady(ctx, inst); err != nil {
                t.Fatalf("wait ready: %v", err)
            }

            r, err := mgr.RCONClient(ctx, inst)
            if err != nil {
                t.Fatalf("rcon: %v", err)
            }
            defer r.Close()

            helper := testenv.NewRCONHelper(r)
            if _, err := helper.SetTime(ctx, "day").Exec(ctx); err != nil {
                t.Fatalf("SetTime: %v", err)
            }

            // TODO: run your actual client against inst.Host:inst.HostServerPort
        })
    }
}
```

Run it with:

```bash
go test ./... -run TestClientCompatibility -v
```

> Note: the `integration_test.go` in this repo currently has `// Xgo:build integration` rather than
> a real `//go:build integration` constraint, so it is **not** actually gated behind a build tag —
> it compiles and runs as part of any `go test ./...` invocation and will attempt to talk to Docker.
> If you restore a real build tag, add `-tags=integration` back to the command above.

Make sure Docker is running and you have enough resources for the Mojang
server(s). You can control the number of parallel tests with `-parallel`
and by limiting the size of your version matrix.

## Notes / Caveats

- This is a simple test harness; for heavy production use, you may want
  more robust logging, metrics, and retry logic.
- If you are testing many versions in parallel, consider capping the
  number of concurrent servers to avoid exhausting CPU/RAM.
- The `itzg/minecraft-server` image supports many additional env vars
  (world type, seeds, etc.). You can pass those via `ServerConfig.ExtraEnv`.
