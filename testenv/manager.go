package testenv

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// ServerConfig describes how to start a test Minecraft server container.
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
	MountDirs  []string          // mount directories (will be mounted to /data/<val>)

	// HostServerPort/HostRCONPort, if non-zero, bind the container's
	// 25565/tcp and 25575/tcp ports to these exact host ports instead of
	// letting Docker choose a random free one (the default, zero-value
	// behavior — unchanged for every existing caller that doesn't set
	// these). Useful for a caller that wants a server it can find again
	// at a predictable, fixed address on a later run (e.g. "is a server
	// already up at this host:port; if not, start one there") rather
	// than a purely disposable, per-run instance. Start returns an error
	// if the requested port is already bound by something else — which
	// is the expected outcome when it's genuinely in use, not a bug to
	// route around here.
	HostServerPort int
	HostRCONPort   int

	// RCONPassword, if non-empty, is used as-is instead of generating a
	// random one. Needed for the same "find it again later" use case
	// HostServerPort/HostRCONPort serve: a caller that only ever
	// discovers a pre-existing server (never calls Start for it) still
	// needs to know its RCON password in advance, which only works if
	// whoever *did* start it was told to use a specific, known password
	// rather than one generated fresh each time. Empty (the default)
	// preserves today's random-generation behavior exactly.
	RCONPassword string
}

// Instance describes a running server container instance.
type Instance struct {
	ID             string
	Name           string
	Version        string
	Host           string
	HostServerPort int
	HostRCONPort   int
	RCONPassword   string
}

// Manager controls Minecraft test server containers.
type Manager interface {
	Start(ctx context.Context, cfg ServerConfig) (*Instance, error)
	WaitReady(ctx context.Context, inst *Instance) error
	Stop(ctx context.Context, inst *Instance, remove bool) error
	Logs(ctx context.Context, containterID string, logOptions client.ContainerLogsOptions) (client.ContainerLogsResult, error)

	RCONClient(ctx context.Context, inst *Instance) (RCON, error)
}

type manager struct {
	cli *client.Client

	// sshTarget, when non-empty, is a DOCKER_HOST=ssh://... value: instead
	// of (unsuccessfully - the underlying client has no real SSH
	// transport, see parseSSHDockerHost) handing this to client.New, Start
	// shells out to open an actual SSH tunnel per container and uses a
	// plain local tcp:// client through that tunnel instead. cli is unused
	// (nil) in this mode - every operation is scoped to one tunneled
	// container via tunnels below.
	sshTarget string

	// tunnels holds the live sshTunnel for each container ID currently
	// running behind one, keyed by Instance.ID. Populated by Start once a
	// container actually exists (not before - there's no ID to key on
	// yet), consulted by clientForContainer, and removed by Stop once the
	// tunnel itself is closed.
	tunnelsMu sync.Mutex
	tunnels   map[string]*sshTunnel
}

// NewManager creates a new Manager using the local Docker daemon
// (DOCKER_HOST, etc), or - if DOCKER_HOST is an ssh:// URI - a Manager that
// opens a dedicated SSH tunnel per container it starts (see Start and
// ssh_tunnel.go) rather than trying to hand that URI to the Docker client
// directly, which doesn't work: this package's vendored Docker client has
// no real SSH transport (confirmed directly - it attempts a plain DNS
// lookup of the host, not an SSH connection), unlike the full docker CLI's
// separate connhelper package.
func NewManager() (Manager, error) {
	if target, ok := parseSSHDockerHost(os.Getenv(client.EnvOverrideHost)); ok {
		return &manager{sshTarget: target, tunnels: map[string]*sshTunnel{}}, nil
	}

	cli, err := client.New(client.FromEnv)
	if err != nil {
		return nil, err
	}
	return &manager{cli: cli, tunnels: map[string]*sshTunnel{}}, nil
}

// clientForContainer returns the Docker API client to use for an operation
// on an already-created container: the per-instance tunneled client if it
// lives behind an SSH tunnel, or the manager's own client otherwise.
func (m *manager) clientForContainer(containerID string) *client.Client {
	m.tunnelsMu.Lock()
	defer m.tunnelsMu.Unlock()
	if t, ok := m.tunnels[containerID]; ok {
		return t.cli
	}
	return m.cli
}

func (m *manager) Start(ctx context.Context, cfg ServerConfig) (*Instance, error) {
	if cfg.Version == "" {
		return nil, errors.New("ServerConfig.Version is required")
	}
	image := cfg.Image
	if image == "" {
		image = "itzg/minecraft-server:latest"
	}

	isSSHTunnel := m.sshTarget != ""
	hasLocalBinds := cfg.DataDir != "" || cfg.ModsDir != "" || cfg.ConfigDir != "" || cfg.OutputDir != "" || len(cfg.MountDirs) > 0

	// cli is resolved once, up front, and used for every Docker API call
	// this function makes - either the manager's own client, or (in SSH
	// tunnel mode) a fresh client dedicated to this one container, dialed
	// through a tunnel opened just below. Nothing past this point should
	// ever reference m.cli directly.
	cli := m.cli
	var tunnel *sshTunnel
	var closeTunnelOnReturn bool
	var removeStageOnReturn bool
	var remoteStageRoot string
	var extraBinds []string
	if isSSHTunnel {
		// cfg.HostServerPort/HostRCONPort are overridden unconditionally
		// here (even if the caller already set them) - SSH tunnel mode
		// must control these itself to guarantee the local-forwarded port
		// and the remote-bound port are the same number (see
		// startSSHTunnel's doc comment), which a caller-supplied value
		// could trivially break.
		var err error
		cfg.HostServerPort, err = freeLocalPort()
		if err != nil {
			return nil, fmt.Errorf("ssh tunnel: %w", err)
		}
		cfg.HostRCONPort, err = freeLocalPort()
		if err != nil {
			return nil, fmt.Errorf("ssh tunnel: %w", err)
		}
		apiPort, err := freeLocalPort()
		if err != nil {
			return nil, fmt.Errorf("ssh tunnel: %w", err)
		}

		tunnel, err = startSSHTunnel(ctx, m.sshTarget, apiPort, cfg.HostServerPort, cfg.HostRCONPort)
		if err != nil {
			return nil, fmt.Errorf("ssh tunnel: %w", err)
		}
		cli = tunnel.cli

		// Closed on every error return below. Start's final success path
		// sets this to false and hands ownership to m.tunnels instead, for
		// Stop to close once the container itself is gone.
		closeTunnelOnReturn = true
		defer func() {
			if closeTunnelOnReturn {
				_ = tunnel.Close()
			}
		}()

		if hasLocalBinds {
			// A fresh random root per container (not derived from anything
			// else already computed here) - keeps this independent of
			// rconPassword/name, which are generated later and shouldn't
			// need reordering just to feed this.
			suffix, err := randomHex(8)
			if err != nil {
				return nil, fmt.Errorf("ssh tunnel: generate remote stage directory name: %w", err)
			}
			remoteStageRoot = "/tmp/mc-client-test-go-" + suffix
			tunnel.remoteStageDir = remoteStageRoot

			// Removed on every error return from Start from here on (via
			// the deferred check below, mirroring closeTunnelOnReturn) -
			// including a failure in the container create/start/inspect
			// steps that come *after* this block, not just a sync failure
			// right here. A sync that fails partway through has still
			// staged *something* on the remote host that needs cleaning
			// up, not just a cleanly-failed one.
			removeStageOnReturn = true
			defer func() {
				if removeStageOnReturn {
					_ = removeRemoteDir(context.Background(), m.sshTarget, remoteStageRoot)
				}
			}()

			binds, err := syncLocalBindsToRemote(ctx, m.sshTarget, remoteStageRoot, cfg)
			if err != nil {
				return nil, fmt.Errorf("ssh tunnel: stage local directories on remote host: %w", err)
			}
			extraBinds = binds

			// The generic per-field bind-building code below must not also
			// try to bind these fields' *original* local paths directly -
			// extraBinds (built from the remote-staged paths instead)
			// already covers them.
			cfg.DataDir, cfg.ModsDir, cfg.ConfigDir, cfg.OutputDir, cfg.MountDirs = "", "", "", "", nil
		}
	}

	host, apiRemote := resolveDaemonHost(cli)
	// Same local-filesystem problem as the SSH-tunnel check above, for a
	// daemon reached directly over tcp:// with no tunnel involved (where
	// isSSHTunnel is false but the daemon is still remote - resolveDaemonHost
	// is what actually knows that here; a tunneled client's DaemonHost() is
	// its own local endpoint, so this check correctly leaves it alone -
	// already handled, before any tunnel was even opened, above).
	if apiRemote && hasLocalBinds {
		return nil, fmt.Errorf("ServerConfig requests local directory binds (DataDir/ModsDir/ConfigDir/OutputDir/MountDirs), but the Docker daemon at %s is remote - these require a daemon with access to this machine's filesystem; unset them, or pre-sync the data to the remote host and reference it with a path valid there, or run against a local daemon", host)
	}

	if cfg.PullImage {
		if err := pullImageIfNeeded(ctx, cli, image); err != nil {
			return nil, fmt.Errorf("pull image: %w", err)
		}
	}

	rconPassword := cfg.RCONPassword
	if rconPassword == "" {
		generated, err := randomHex(16)
		if err != nil {
			return nil, fmt.Errorf("generate RCON password: %w", err)
		}
		rconPassword = generated
	}

	env := []string{
		"EULA=TRUE",
		"VERSION=" + cfg.Version,
		"ONLINE_MODE=" + boolToString(cfg.OnlineMode),
		"ENABLE_RCON=TRUE",
		"RCON_PASSWORD=" + rconPassword,
	}
	for k, v := range cfg.ExtraEnv {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}

	serverPort, err := network.ParsePort("25565/tcp")
	if err != nil {
		return nil, err
	}
	rconPort, err := network.ParsePort("25575/tcp")
	if err != nil {
		return nil, err
	}

	exposedPorts := network.PortSet{
		serverPort: struct{}{},
		rconPort:   struct{}{},
	}

	allHostIP, err := netip.ParseAddr("0.0.0.0")
	if err != nil {
		return nil, err
	}

	localHostIP, err := netip.ParseAddr("127.0.0.1")
	if err != nil {
		return nil, err
	}

	// RCON binds to loopback-only by default - deliberately, since it's an
	// admin interface and this keeps it off the LAN for the common local-
	// daemon case. Against a directly-reached remote daemon (plain tcp://,
	// no tunnel), loopback is the *remote* machine's own loopback, which
	// nothing - including this test runner - could ever reach, so RCON
	// must bind to all interfaces there instead. Callers pointing at a
	// remote daemon that way are already responsible for that daemon's own
	// network exposure (same as exposing the Docker API itself remotely);
	// this follows the same tradeoff for RCON specifically.
	//
	// An SSH tunnel is different: it already provides the remote access
	// path by forwarding to the remote host's own loopback (see
	// ssh_tunnel.go), so RCON staying loopback-only there is both correct
	// and strictly better - it's never exposed beyond the tunnel itself,
	// not even password-protected-but-open on the remote LAN.
	rconHostIP := localHostIP
	if apiRemote && !isSSHTunnel {
		rconHostIP = allHostIP
	}

	serverHostPort := ""
	if cfg.HostServerPort != 0 {
		serverHostPort = strconv.Itoa(cfg.HostServerPort)
	}
	rconHostPort := ""
	if cfg.HostRCONPort != 0 {
		rconHostPort = strconv.Itoa(cfg.HostRCONPort)
	}

	portBindings := network.PortMap{
		serverPort: []network.PortBinding{
			{HostIP: allHostIP, HostPort: serverHostPort},
		},
		rconPort: []network.PortBinding{
			{HostIP: rconHostIP, HostPort: rconHostPort},
		},
	}

	hostCfg := &container.HostConfig{
		PortBindings: portBindings,
	}
	if cfg.DataDir != "" {
		hostCfg.Binds = []string{
			fmt.Sprintf("%s:/data", cfg.DataDir),
		}
	}
	if cfg.ModsDir != "" {
		hostCfg.Binds = append(hostCfg.Binds,
			fmt.Sprintf("%s:/data/mods", cfg.ModsDir),
		)
	}
	if cfg.ConfigDir != "" {
		hostCfg.Binds = append(hostCfg.Binds,
			fmt.Sprintf("%s:/data/config", cfg.ConfigDir),
		)
	}
	if cfg.OutputDir != "" {
		hostCfg.Binds = append(hostCfg.Binds,
			fmt.Sprintf("%s:/data/output", cfg.OutputDir),
		)
	}

	for _, mount := range cfg.MountDirs {
		hostCfg.Binds = append(hostCfg.Binds, fmt.Sprintf("%s:/data/%s", mount, mount))
	}
	// extraBinds (SSH tunnel mode only, populated above when hasLocalBinds)
	// already uses remote-staged paths for exactly the fields the generic
	// per-field code above would otherwise have handled - they were
	// cleared on cfg before reaching any of it, so there's no overlap.
	hostCfg.Binds = append(hostCfg.Binds, extraBinds...)

	name := cfg.NamePrefix + "mc-" + strings.ReplaceAll(cfg.Version, ".", "-") +
		"-" + strings.ToLower(mustShortID(rconPassword))

	contCfg := &container.Config{
		Image:        image,
		Env:          env,
		ExposedPorts: exposedPorts,
	}

	createOptions := client.ContainerCreateOptions{
		HostConfig: hostCfg,
		Config:     contCfg,
		Name:       name,
	}

	createResp, err := cli.ContainerCreate(
		ctx,
		createOptions,
	)
	if err != nil {
		return nil, fmt.Errorf("container create: %w", err)
	}

	// From here on, a failure must still clean up the now-created container
	// (and, in SSH tunnel mode, the tunnel) rather than leaking it the way a
	// bare early return would - mirrors Stop's own "always attempt removal"
	// philosophy, just applied to a container that never finished starting.
	cleanupOnFailure := func() {
		removeCtx, cancel := context.WithTimeout(context.Background(), orphanFallbackRemoveTimeout)
		defer cancel()
		_, _ = cli.ContainerRemove(removeCtx, createResp.ID, client.ContainerRemoveOptions{RemoveVolumes: true, Force: true})
	}

	if containerStartResult, err := cli.ContainerStart(ctx, createResp.ID, client.ContainerStartOptions{}); err != nil {
		cleanupOnFailure()
		return nil, fmt.Errorf("container start: %w", err)
	} else {
		fmt.Println("ContainerStart result: ", containerStartResult)
	}

	containerInspectResult, err := cli.ContainerInspect(ctx, createResp.ID, client.ContainerInspectOptions{})
	if err != nil {
		cleanupOnFailure()
		return nil, fmt.Errorf("container inspect: %w", err)
	}

	serverBindings := containerInspectResult.Container.NetworkSettings.Ports[serverPort]
	if len(serverBindings) == 0 {
		cleanupOnFailure()
		return nil, fmt.Errorf("no host port mapped for %s", serverPort)
	}
	rconBindings := containerInspectResult.Container.NetworkSettings.Ports[rconPort]
	if len(rconBindings) == 0 {
		cleanupOnFailure()
		return nil, fmt.Errorf("no host port mapped for %s", rconPort)
	}

	hostServerPort, err := strconv.Atoi(serverBindings[0].HostPort)
	if err != nil {
		cleanupOnFailure()
		return nil, fmt.Errorf("parse host server port: %w", err)
	}
	hostRCONPort, err := strconv.Atoi(rconBindings[0].HostPort)
	if err != nil {
		cleanupOnFailure()
		return nil, fmt.Errorf("parse host RCON port: %w", err)
	}

	inst := &Instance{
		ID:             createResp.ID,
		Name:           name,
		Version:        cfg.Version,
		Host:           host,
		HostServerPort: hostServerPort,
		HostRCONPort:   hostRCONPort,
		RCONPassword:   rconPassword,
	}

	if isSSHTunnel {
		// Success: the tunnel (and any remote-staged directories) are no
		// longer this function's to clean up - they now belong to this
		// instance for as long as the container lives, and Stop is what
		// closes/removes them (remoteStageDir, carried on tunnel itself).
		closeTunnelOnReturn = false
		removeStageOnReturn = false
		m.tunnelsMu.Lock()
		m.tunnels[createResp.ID] = tunnel
		m.tunnelsMu.Unlock()
	}

	return inst, nil
}

// WaitReady waits until the Minecraft server is ready to accept RCON commands.
//
// Strategy:
//   - Ensure ctx has a deadline (default 3 minutes if none supplied).
//   - In a loop, try to create an RCON client and execute a simple command ("list").
//   - Return nil as soon as that succeeds, or an error if the timeout is reached.
//
// This makes readiness align directly with what tests care about: RCON being usable.
func (m *manager) WaitReady(ctx context.Context, inst *Instance) error {
	// Ensure we have a deadline so we don't spin forever.
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
	}

	// Poll RCON until we can successfully run "list".
	// Also monitor container health to detect crashes early.
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	// Health check ticker - check container state more frequently
	healthTicker := time.NewTicker(1 * time.Second)
	defer healthTicker.Stop()

	var lastErr error
	var lastHealthErr error
	_ = lastHealthErr // may be used for future enhanced error reporting

	for {
		select {
		case <-ctx.Done():
			// Check container state one last time before reporting timeout
			if state, err := m.getContainerState(ctx, inst.ID); err == nil {
				if !state.Running {
					return fmt.Errorf("container exited with code %d: %s (last RCON error: %v)",
						state.ExitCode, state.Error, lastErr)
				}
			}
			if lastErr != nil {
				return fmt.Errorf("WaitReady timeout, last error: %w", lastErr)
			}
			return fmt.Errorf("WaitReady timeout: %w", ctx.Err())

		case <-healthTicker.C:
			// Check if container is still running
			state, err := m.getContainerState(ctx, inst.ID)
			if err != nil {
				lastHealthErr = fmt.Errorf("failed to check container state: %w", err)
				continue
			}

			if !state.Running {
				// Container has exited - get logs for debugging
				logs := m.getRecentLogs(ctx, inst.ID, 250)
				return fmt.Errorf("container exited during startup with code %d: %s\n\nRecent logs:\n%s",
					state.ExitCode, state.Error, logs)
			}

			// Check for OOMKilled specifically
			if state.OOMKilled {
				logs := m.getRecentLogs(ctx, inst.ID, 250)
				return fmt.Errorf("container killed by OOM (out of memory)\n\nRecent logs:\n%s", logs)
			}

		case <-ticker.C:
			r, err := m.RCONClient(ctx, inst)
			if err != nil {
				// Dial failed; try again on next tick.
				lastErr = err
				continue
			}

			// Try a simple, cheap command; if this works, the server is ready.
			if _, err := r.Exec(ctx, "list"); err != nil {
				lastErr = err
				_ = r.Close()
				continue
			}

			_ = r.Close()
			return nil
		}
	}
}

// containerState holds relevant container state information
type containerState struct {
	Running   bool
	ExitCode  int
	Error     string
	OOMKilled bool
}

// getContainerState retrieves the current container state
func (m *manager) getContainerState(ctx context.Context, containerID string) (*containerState, error) {
	inspect, err := m.clientForContainer(containerID).ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return nil, fmt.Errorf("inspect container: %w", err)
	}

	return &containerState{
		Running:   inspect.Container.State.Running,
		ExitCode:  inspect.Container.State.ExitCode,
		Error:     inspect.Container.State.Error,
		OOMKilled: inspect.Container.State.OOMKilled,
	}, nil
}

// getRecentLogs retrieves the last N lines of container logs for debugging
func (m *manager) getRecentLogs(ctx context.Context, containerID string, lines int) string {
	tailLines := fmt.Sprintf("%d", lines)
	containerReader, err := m.Logs(ctx, containerID, client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       tailLines,
	})
	if err != nil {
		return fmt.Sprintf("(failed to retrieve logs: %v)", err)
	}
	defer containerReader.Close()

	// containerReader is a multiplexed stream, we need to seperate and remerge the stream to remove multiplexing metadata
	var reader io.Reader
	mergedStdErr := &bytes.Buffer{}

	_, err = stdcopy.StdCopy(mergedStdErr, mergedStdErr, containerReader)
	if err != nil {
		reader = containerReader
	} else {
		reader = mergedStdErr
	}

	// Read all logs
	buf := new(strings.Builder)
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		buf.WriteString(scanner.Text())
		buf.WriteString("\n")
	}

	if buf.Len() == 0 {
		return "(no logs available)"
	}
	return buf.String()
}

// WaitReady waits until the Minecraft server is ready to accept connections.
// Implementation: tail container logs and look for a "Done (" line.
func (m *manager) WaitReady_Logger(ctx context.Context, inst *Instance) error {
	// Basic safety timeout if caller forgot one.
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
	}

	reader, err := m.Logs(ctx, inst.ID, client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
		Timestamps: false,
		Since:      "",
		Tail:       "0",
	})
	if err != nil {
		return fmt.Errorf("container logs: %w", err)
	}
	defer reader.Close()

	readyMarkers := []string{
		"Done (",                     // vanilla
		`For help, type "help"`,      // often appears around ready
		"Preparing spawn area: 100%", // some versions
	}

	scanner := bufio.NewScanner(reader)

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait ready timeout: %w", ctx.Err())
		default:
		}

		if !scanner.Scan() {
			if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
				return fmt.Errorf("log scan error: %w", err)
			}
			// Small delay to avoid tight-looping if logs are quiet.
			time.Sleep(500 * time.Millisecond)
			continue
		}

		line := scanner.Text()
		for _, marker := range readyMarkers {
			if strings.Contains(line, marker) {
				return nil
			}
		}
	}
}

// stopGracePeriod is how long Docker waits after SIGTERM before it
// SIGKILLs a container being stopped. 10s (not the previous 30s): test
// containers don't need a full graceful Minecraft server shutdown (callers
// already discard/regenerate world data between runs), and a shorter grace
// period leaves real headroom below a caller's own context deadline - the
// previous 30s grace period matched a common 30s caller timeout exactly,
// so a container that was merely slow (not stuck) to respond to SIGTERM
// would race the caller's own ctx and report a spurious "context deadline
// exceeded" even though Docker would have finished stopping it moments
// later.
const stopGracePeriod = 10 * time.Second

// orphanFallbackRemoveTimeout bounds the fresh, independent context the
// removal fallback below uses - never the caller's own ctx, which may
// already be past its deadline by the time Stop reaches this point (e.g.
// if the graceful stop above consumed the caller's entire timeout budget).
// Reusing an already-expired ctx here would fail identically without ever
// actually attempting removal, defeating the whole point of the fallback.
const orphanFallbackRemoveTimeout = 15 * time.Second

func (m *manager) Stop(ctx context.Context, inst *Instance, remove bool) error {
	cli := m.clientForContainer(inst.ID)

	tVal := int(stopGracePeriod.Seconds())
	stopResult, err := cli.ContainerStop(ctx, inst.ID, client.ContainerStopOptions{
		Timeout: &tVal,
	})
	var stopErr error
	if err != nil {
		stopErr = fmt.Errorf("container stop: %w", err)
	} else {
		fmt.Println("ContainerStop result: ", stopResult)
	}

	if !remove {
		// The container (and, if it has one, its SSH tunnel) may still be
		// inspected/restarted/logged by a later call, so neither is torn
		// down here - only a remove=true Stop means "done with this one".
		return stopErr
	}

	// Always attempt removal, even if the graceful stop above errored or
	// timed out - a container that's merely slow to respond to SIGTERM (or
	// whose stop confirmation raced the caller's own context deadline)
	// still needs to be removed, not left as an orphan for the next test
	// run to trip over. Force:true makes this work regardless of whether
	// the container ended up stopped, still running, or somewhere in
	// between - functionally the same as `docker rm -f`.
	removeCtx, removeCancel := context.WithTimeout(context.Background(), orphanFallbackRemoveTimeout)
	defer removeCancel()
	removeResult, err := cli.ContainerRemove(removeCtx, inst.ID, client.ContainerRemoveOptions{
		RemoveVolumes: true,
		Force:         true,
	})

	// Close and forget this container's SSH tunnel (if any) regardless of
	// how removal went - a remove=true Stop means the caller is done with
	// this container either way, and the exact same orphaned-process risk
	// that motivated Start's own "always clean up on failure" applies here
	// to the tunnel's ssh subprocess, not just the container.
	m.tunnelsMu.Lock()
	tunnel, hadTunnel := m.tunnels[inst.ID]
	delete(m.tunnels, inst.ID)
	m.tunnelsMu.Unlock()
	if hadTunnel {
		_ = tunnel.Close()
		if tunnel.remoteStageDir != "" {
			stageCtx, stageCancel := context.WithTimeout(context.Background(), orphanFallbackRemoveTimeout)
			_ = removeRemoteDir(stageCtx, m.sshTarget, tunnel.remoteStageDir)
			stageCancel()
		}
	}

	if err != nil {
		if stopErr != nil {
			return fmt.Errorf("%w (remove also failed: %v)", stopErr, err)
		}
		return fmt.Errorf("container remove: %w", err)
	}
	fmt.Println("ContainerRemove result: ", removeResult)

	// Report the original stop error even though removal ultimately
	// succeeded - it's informational (something about the graceful
	// shutdown path was off) but no longer means the container leaked.
	return stopErr
}

func (m *manager) Logs(ctx context.Context, containerID string, opts client.ContainerLogsOptions) (client.ContainerLogsResult, error) {
	reader, err := m.clientForContainer(containerID).ContainerLogs(ctx, containerID, opts)
	if err != nil {
		return nil, fmt.Errorf("container logs: %w", err)
	}

	return reader, nil
}

func (m *manager) RCONClient(ctx context.Context, inst *Instance) (RCON, error) {
	return newRCONClient(ctx, inst.Host, inst.HostRCONPort, inst.RCONPassword)
}

// pullImageIfNeeded pulls image via cli if it is not already present on
// whichever daemon cli talks to (not a method on *manager: Start resolves
// cli itself - m.cli, or a per-instance tunneled client - before this is
// ever called, and it must use exactly that client, not reach past it back
// to m.cli).
func pullImageIfNeeded(ctx context.Context, cli *client.Client, image string) error {
	filters := client.Filters{}
	filters.Add("reference", image)

	// Quick check if image exists locally
	imageListResult, err := cli.ImageList(ctx, client.ImageListOptions{
		Filters: filters,
	})
	if err == nil && len(imageListResult.Items) > 0 {
		// Already present
		return nil
	}

	rc, err := cli.ImagePull(ctx, image, client.ImagePullOptions{})
	if err != nil {
		return err
	}
	defer rc.Close()
	// Drain output to actually complete the pull.
	_, _ = io.Copy(io.Discard, rc)
	return nil
}

// Helpers

// resolveDaemonHost inspects cli's resolved Docker daemon host (whatever
// client.FromEnv set it to - DOCKER_HOST, or the platform default when
// unset) and returns the hostname test code should use to reach a
// container's published ports, plus whether that daemon is non-local.
//
// A unix socket or Windows named pipe means the daemon is on this machine -
// published ports are reachable at 127.0.0.1 regardless of the socket's own
// path. A tcp/http(s)/ssh host whose hostname is itself localhost/
// 127.0.0.1/::1 is still local for this purpose too (e.g.
// DOCKER_HOST=tcp://127.0.0.1:2375, a common way to test against a local
// daemon over TCP/TLS rather than genuinely pointing elsewhere). Anything
// else is a genuinely remote daemon, and both the host this function
// returns and the remote bool it returns matter beyond just this function:
// see Start's use of remote for RCON's bind address and for rejecting
// local-filesystem bind mounts that a remote daemon can't honor.
func resolveDaemonHost(cli *client.Client) (host string, remote bool) {
	raw := cli.DaemonHost()
	u, err := url.Parse(raw)
	if err != nil {
		return "127.0.0.1", false
	}
	switch u.Scheme {
	case "unix", "npipe", "":
		return "127.0.0.1", false
	}
	switch u.Hostname() {
	case "", "localhost", "127.0.0.1", "::1":
		return "127.0.0.1", false
	default:
		return u.Hostname(), true
	}
}

func boolToString(b bool) string {
	if b {
		return "TRUE"
	}
	return "FALSE"
}

func randomHex(nBytes int) (string, error) {
	buf := make([]byte, nBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func mustShortID(s string) string {
	if len(s) < 8 {
		return s
	}
	return s[:8]
}
