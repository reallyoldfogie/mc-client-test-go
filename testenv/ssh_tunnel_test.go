package testenv

import (
	"net"
	"os/exec"
	"strconv"
	"testing"
	"time"
)

func TestParseSSHDockerHost(t *testing.T) {
	tests := []struct {
		name   string
		raw    string
		want   string
		wantOK bool
	}{
		{"empty", "", "", false},
		{"ssh bare host", "ssh://do-docker-host2", "ssh://do-docker-host2", true},
		{"ssh with user and port", "ssh://root@1.2.3.4:2222", "ssh://root@1.2.3.4:2222", true},
		{"unix socket", "unix:///var/run/docker.sock", "", false},
		{"tcp", "tcp://10.0.0.5:2375", "", false},
		{"malformed", "://not a url", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseSSHDockerHost(tt.raw)
			if ok != tt.wantOK {
				t.Fatalf("parseSSHDockerHost(%q) ok = %v, want %v", tt.raw, ok, tt.wantOK)
			}
			if got != tt.want {
				t.Errorf("parseSSHDockerHost(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestFreeLocalPort_ReturnsAnActuallyBindablePort(t *testing.T) {
	port, err := freeLocalPort()
	if err != nil {
		t.Fatalf("freeLocalPort: %v", err)
	}
	if port <= 0 || port > 65535 {
		t.Fatalf("freeLocalPort returned out-of-range port %d", port)
	}

	// Immediately rebinding it should succeed - freeLocalPort released it
	// rather than holding it open.
	l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatalf("rebinding port %d returned by freeLocalPort: %v", port, err)
	}
	l.Close()
}

func TestFreeLocalPort_DistinctAcrossCalls(t *testing.T) {
	seen := map[int]bool{}
	for i := 0; i < 10; i++ {
		port, err := freeLocalPort()
		if err != nil {
			t.Fatalf("freeLocalPort: %v", err)
		}
		if seen[port] {
			t.Fatalf("freeLocalPort returned port %d twice in %d calls", port, i+1)
		}
		seen[port] = true
	}
}

// wrapAsTunnel starts cmd and wraps it exactly as startSSHTunnel wraps the
// real ssh process - including the background Wait()/waitDone reaper -
// so processExited/Close can be tested against an arbitrary subprocess
// without any real ssh/Docker involved.
func wrapAsTunnel(t *testing.T, cmd *exec.Cmd) *sshTunnel {
	t.Helper()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start fake subprocess: %v", err)
	}
	tunnel := &sshTunnel{cmd: cmd, waitDone: make(chan struct{})}
	go func() {
		tunnel.waitErr = cmd.Wait()
		close(tunnel.waitDone)
	}()
	return tunnel
}

// fakeTunnel wraps a long-lived, harmless subprocess (sleep) - for tests
// that need something still running when the test body starts.
func fakeTunnel(t *testing.T) *sshTunnel {
	t.Helper()
	return wrapAsTunnel(t, exec.Command("sleep", "30"))
}

func TestSSHTunnel_ProcessExited_FalseWhileRunning(t *testing.T) {
	tunnel := fakeTunnel(t)
	defer tunnel.Close()

	exited, err := tunnel.processExited()
	if exited {
		t.Errorf("processExited = true while the process is still running (err: %v)", err)
	}
}

func TestSSHTunnel_Close_KillsTheProcess(t *testing.T) {
	tunnel := fakeTunnel(t)

	if err := tunnel.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Closing again must be a safe no-op, not a second kill attempt on an
	// already-dead process (Stop's "always attempt cleanup" callers may
	// legitimately call this more than once).
	if err := tunnel.Close(); err != nil {
		t.Errorf("second Close: %v, want nil (should be a no-op)", err)
	}

	exited, _ := tunnel.processExited()
	if !exited {
		t.Error("processExited = false after Close, want true")
	}
}

func TestSSHTunnel_ProcessExited_TrueAfterNaturalExit(t *testing.T) {
	tunnel := wrapAsTunnel(t, exec.Command("true")) // exits immediately on its own

	// Give the background reaper goroutine a moment to actually observe the
	// exit before polling - Start() only guarantees the process has begun,
	// not that Wait() has already returned.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if exited, _ := tunnel.processExited(); exited {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("processExited never reported true for a process that should have exited immediately")
}
