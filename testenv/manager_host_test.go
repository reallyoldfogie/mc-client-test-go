package testenv

import (
	"testing"

	"github.com/moby/moby/client"
)

// newClientWithHost builds a *client.Client pinned to host without dialing
// anything - client.New only parses/validates the host URL and configures
// an HTTP transport; it doesn't make a network call until a request method
// is actually invoked. Safe to use in a unit test with no Docker daemon
// available.
func newClientWithHost(t *testing.T, host string) *client.Client {
	t.Helper()
	cli, err := client.New(client.WithHost(host))
	if err != nil {
		t.Fatalf("client.New(WithHost(%q)): %v", host, err)
	}
	return cli
}

func TestResolveDaemonHost_UnixSocketIsLocal(t *testing.T) {
	cli := newClientWithHost(t, "unix:///var/run/docker.sock")
	host, remote := resolveDaemonHost(cli)
	if remote {
		t.Error("remote = true for a unix socket, want false")
	}
	if host != "127.0.0.1" {
		t.Errorf("host = %q, want 127.0.0.1", host)
	}
}

func TestResolveDaemonHost_TCPLocalhostIsLocal(t *testing.T) {
	tests := []string{
		"tcp://127.0.0.1:2375",
		"tcp://localhost:2375",
		"tcp://[::1]:2375",
	}
	for _, raw := range tests {
		cli := newClientWithHost(t, raw)
		host, remote := resolveDaemonHost(cli)
		if remote {
			t.Errorf("resolveDaemonHost(%q): remote = true, want false", raw)
		}
		if host != "127.0.0.1" {
			t.Errorf("resolveDaemonHost(%q): host = %q, want 127.0.0.1", raw, host)
		}
	}
}

func TestResolveDaemonHost_TCPRemoteHostIsRemote(t *testing.T) {
	cli := newClientWithHost(t, "tcp://10.0.0.5:2375")
	host, remote := resolveDaemonHost(cli)
	if !remote {
		t.Error("remote = false for a non-local tcp host, want true")
	}
	if host != "10.0.0.5" {
		t.Errorf("host = %q, want 10.0.0.5", host)
	}
}

func TestResolveDaemonHost_SSHRemoteHostIsRemote(t *testing.T) {
	cli := newClientWithHost(t, "ssh://user@build-host:22")
	host, remote := resolveDaemonHost(cli)
	if !remote {
		t.Error("remote = false for an ssh host, want true")
	}
	if host != "build-host" {
		t.Errorf("host = %q, want build-host", host)
	}
}
