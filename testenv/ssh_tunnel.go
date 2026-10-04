package testenv

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"time"

	"github.com/moby/moby/client"
)

// remoteDockerSocket is the standard location of the Docker daemon's unix
// socket on virtually every Linux Docker install - what the tunnel's first
// -L forward targets. There's no way to discover a non-default path
// remotely without already having a working connection, so this isn't
// configurable today; a caller with a genuinely nonstandard remote socket
// path isn't served by this feature yet.
const remoteDockerSocket = "/var/run/docker.sock"

// sshTunnelReadyTimeout/sshTunnelReadyPoll bound how long startSSHTunnel
// waits for the tunnel to actually be usable (ssh connected, authenticated,
// and the forwarded Docker API socket answering) before giving up.
const (
	sshTunnelReadyTimeout = 20 * time.Second
	sshTunnelReadyPoll    = 300 * time.Millisecond
)

// parseSSHDockerHost reports whether raw (as DOCKER_HOST would be set) is an
// ssh:// target, returning it unchanged if so - OpenSSH (7.3+) accepts a
// full ssh://[user@]host[:port] URI directly as its destination argument,
// so there's no need to pick it apart into user/host/port ourselves.
func parseSSHDockerHost(raw string) (target string, ok bool) {
	if raw == "" {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "ssh" {
		return "", false
	}
	return raw, true
}

// sshTunnel is one SSH connection, opened for exactly one Start'd container,
// forwarding three local ports to the remote host: the Docker daemon's unix
// socket (so the Docker API client for this container's whole lifecycle can
// be a perfectly ordinary local tcp:// client), and the container's own
// game-server and RCON ports, each forwarded to the *same* port number on
// the remote host's own loopback - see startSSHTunnel for why picking
// matching numbers on both ends avoids needing any separate "local port"
// bookkeeping anywhere else in this package. Test code outside this package
// never needs to know a tunnel is involved at all: Instance.Host ends up
// "127.0.0.1" and HostServerPort/HostRCONPort are the very ports this
// tunnel forwards, so every existing caller that builds a connection
// address from those fields already does the right thing unmodified.
type sshTunnel struct {
	cmd    *exec.Cmd
	stderr *bytes.Buffer
	cli    *client.Client

	apiPort    int
	serverPort int
	rconPort   int

	// waitDone is closed once the background goroutine startSSHTunnel
	// starts right after cmd.Start() has reaped the process via cmd.Wait(),
	// at which point waitErr is valid. This - not cmd.Process.Signal(0) -
	// is the reliable way to ask "has this exited yet" for a process we
	// don't want to block waiting for: Signal(0) still succeeds against an
	// exited-but-not-yet-reaped zombie (confirmed directly - a test using
	// it against a process that exits immediately on its own, with nothing
	// else calling Wait, never observed it as exited), since nothing
	// reaped it to make that exit visible. cmd.Wait must only ever be
	// called once per process, so this goroutine is the single owner of
	// that call; Close does not call it again.
	waitDone chan struct{}
	waitErr  error

	// remoteStageDir, if non-empty, is the root directory this tunnel's
	// container had local directory binds (ModsDir, ConfigDir, etc.)
	// staged under via syncLocalBindsToRemote - Stop removes it (see
	// removeRemoteDir) once the container itself is gone.
	remoteStageDir string
}

// startSSHTunnel opens target (an ssh:// URI, passed straight through to
// the ssh binary as its destination) with three local port forwards, and
// waits until the forwarded Docker API socket actually answers a Ping
// before returning - a tunnel that merely connected but can't yet reach the
// remote daemon (wrong socket path, daemon not running, auth succeeded but
// something else is wrong) is caught here, not by a confusing failure three
// calls later in Start.
func startSSHTunnel(ctx context.Context, target string, apiPort, serverPort, rconPort int) (*sshTunnel, error) {
	if _, err := exec.LookPath("ssh"); err != nil {
		return nil, fmt.Errorf("ssh tunnel: %w (is an ssh client installed?)", err)
	}

	args := []string{
		"-N",                  // no remote command - this connection exists only to forward ports
		"-o", "BatchMode=yes", // never prompt (password, passphrase, etc.) - fail fast instead of hanging
		"-o", "StrictHostKeyChecking=accept-new", // don't hang on an interactive host-key prompt; still verifies against known_hosts on repeat connections
		"-o", "ExitOnForwardFailure=yes", // exit immediately if any -L binding fails, instead of staying up half-broken
		"-L", fmt.Sprintf("127.0.0.1:%d:%s", apiPort, remoteDockerSocket),
		"-L", fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", serverPort, serverPort),
		"-L", fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", rconPort, rconPort),
		target,
	}

	cmd := exec.Command("ssh", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("ssh tunnel: start: %w", err)
	}

	t := &sshTunnel{
		cmd:        cmd,
		stderr:     &stderr,
		apiPort:    apiPort,
		serverPort: serverPort,
		rconPort:   rconPort,
		waitDone:   make(chan struct{}),
	}
	go func() {
		t.waitErr = cmd.Wait()
		close(t.waitDone)
	}()

	cli, err := client.New(client.WithHost(fmt.Sprintf("tcp://127.0.0.1:%d", apiPort)))
	if err != nil {
		_ = t.Close()
		return nil, fmt.Errorf("ssh tunnel: build tunneled Docker client: %w", err)
	}
	t.cli = cli

	if err := waitTunnelReady(ctx, t); err != nil {
		_ = t.Close()
		return nil, err
	}

	return t, nil
}

// waitTunnelReady polls Ping on t's tunneled client until it succeeds, the
// process exits (ssh gave up - auth failure, unreachable host, a forward
// binding conflict, etc.), or sshTunnelReadyTimeout elapses.
func waitTunnelReady(ctx context.Context, t *sshTunnel) error {
	deadline := time.Now().Add(sshTunnelReadyTimeout)
	var lastErr error
	for {
		if exited, waitErr := t.processExited(); exited {
			return fmt.Errorf("ssh tunnel: ssh exited before becoming ready: %v\nstderr:\n%s", waitErr, t.stderr.String())
		}

		pingCtx, cancel := context.WithTimeout(ctx, sshTunnelReadyPoll)
		_, err := t.cli.Ping(pingCtx, client.PingOptions{})
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err

		if !time.Now().Before(deadline) {
			return fmt.Errorf("ssh tunnel: not ready after %s, last error: %w\nstderr:\n%s", sshTunnelReadyTimeout, lastErr, t.stderr.String())
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		time.Sleep(sshTunnelReadyPoll)
	}
}

// processExited reports whether the ssh process has already exited, without
// blocking - used by waitTunnelReady's poll loop to fail fast on a dead
// tunnel instead of waiting out the full timeout. See sshTunnel.waitDone's
// doc comment for why this checks that channel rather than probing the
// process directly.
func (t *sshTunnel) processExited() (bool, error) {
	select {
	case <-t.waitDone:
		return true, t.waitErr
	default:
		return false, nil
	}
}

// sshTunnelCloseWait bounds how long Close waits for the killed process to
// actually be reaped (by startSSHTunnel's background goroutine) before
// giving up on waiting and returning anyway - killing it is what matters
// for freeing the port/connection; a slow reap shouldn't block the caller.
const sshTunnelCloseWait = 5 * time.Second

// Close terminates the tunnel's ssh process and waits (briefly) for it to
// be reaped. Always safe to call, including more than once, and including
// on a tunnel that failed to become ready - cmd.Process is non-nil as soon
// as Start succeeded, which is the only path that constructs a *sshTunnel
// at all.
func (t *sshTunnel) Close() error {
	if t.cmd.Process == nil {
		return nil
	}
	if exited, _ := t.processExited(); exited {
		return nil
	}
	if err := t.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("ssh tunnel: kill: %w", err)
	}
	select {
	case <-t.waitDone:
	case <-time.After(sshTunnelCloseWait):
	}
	return nil
}

// freeLocalPort asks the OS for a currently-unused TCP port on 127.0.0.1 by
// binding to port 0 and immediately releasing it. Inherently a best-effort
// reservation (something else could grab the same port before ssh binds
// it) - the same risk profile every "ask the OS for a free port" pattern
// has, and ExitOnForwardFailure=yes at least turns a lost race into a clear
// startSSHTunnel error rather than a silent wrong-port connection.
func freeLocalPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("find free local port: %w", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
