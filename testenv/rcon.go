package testenv

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/gorcon/rcon"
)

// execMu serializes Exec calls per target server address, across every
// rconClient dialed to that address - not just within one instance.
// Necessary because the underlying gorcon.Conn.Execute isn't safe for
// concurrent use even on a single connection (no locking around its own
// write-then-read), and because separate connections to the SAME server
// aren't safe to use concurrently either: confirmed live against a
// shared-server RL training run with 4 bots, each with its own separate
// RCON connection, teleporting concurrently via RCON at the start of
// each episode - the logged response for one bot's own `tp` command
// contained OTHER bots' "Teleported X to Y" confirmation text
// concatenated into it, meaning the server's own command-feedback
// capture mixed output across commands that were in flight to it at the
// same time, regardless of which connection carried each one. Keyed by
// address (not global) so unrelated servers a test suite talks to don't
// serialize against each other.
var (
	execMuMu sync.Mutex
	execMu   = map[string]*sync.Mutex{}
)

// execMutexFor returns the shared mutex for addr, creating it on first
// use. Never removed - the number of distinct addresses a process talks
// to over its lifetime is small and bounded in practice (one or a
// handful of test/training servers), not worth the complexity of
// eviction.
func execMutexFor(addr string) *sync.Mutex {
	execMuMu.Lock()
	defer execMuMu.Unlock()
	mu, ok := execMu[addr]
	if !ok {
		mu = &sync.Mutex{}
		execMu[addr] = mu
	}
	return mu
}

// RCON abstracts communicating with the server via RCON.
type RCON interface {
	Exec(ctx context.Context, cmd string) (string, error)
	Close() error
	Reconnect(ctx context.Context) error
}

type rconClient struct {
	conn *rcon.Conn
	addr string
	pwd  string
}

// DialRCON connects directly to an already-running server's RCON port,
// without spinning up a managed test instance - for callers (e.g.
// mc-agent's cmd/agent/main.go) that already have a running server and
// just need an RCONHelper.
func DialRCON(ctx context.Context, host string, port int, password string) (RCONHelper, error) {
	r, err := newRCONClient(ctx, host, port, password)
	if err != nil {
		return nil, err
	}
	return NewRCONHelper(r), nil
}

// newRCONClient dials RCON and returns an RCON wrapper.
func newRCONClient(ctx context.Context, host string, port int, password string) (RCON, error) {
	addr := fmt.Sprintf("%s:%d", host, port)

	r := &rconClient{
		addr: addr,
		pwd:  password,
	}

	err := r.connect(ctx)
	if err != nil {
		return nil, err
	}

	return r, nil
}

func (c *rconClient) connect(ctx context.Context) error {
	// Use a dial timeout; gorcon's Dial doesn't take a context, so we just time-box it.
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	var conn *rcon.Conn
	errCh := make(chan error, 1)
	go func() {
		c, err := rcon.Dial(c.addr, c.pwd)
		if err != nil {
			errCh <- err
			return
		}
		conn = c
		errCh <- nil
	}()

	select {
	case <-dialCtx.Done():
		return fmt.Errorf("rcon dial timeout: %w", dialCtx.Err())
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("rcon dial: %w", err)
		}
	}

	c.conn = conn
	return nil
}

func (c *rconClient) Reconnect(ctx context.Context) error {
	return c.connect(ctx)
}

func (c *rconClient) Exec(ctx context.Context, cmd string) (response string, err error) {
	// Held for the full write-request/read-response round trip below, not
	// just c.conn.Execute's own call - see execMutexFor's doc comment for
	// why this needs to serialize across every connection to c.addr, not
	// just calls on this one rconClient.
	mu := execMutexFor(c.addr)
	mu.Lock()
	defer mu.Unlock()

	defer func() {
		log.Printf("RCON command response: %s, err: %#v", response, err)
	}()
	// gorcon doesn't take a context; wrap in a goroutine with timeout.
	execCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	type resp struct {
		reply string
		err   error
	}

	ch := make(chan resp, 1)
	log.Printf("Executing RCON command: %s", cmd)

	go func() {
		r, err := c.conn.Execute(cmd)
		ch <- resp{reply: r, err: err}
	}()

	select {
	case <-execCtx.Done():
		return "", fmt.Errorf("rcon exec timeout: %w", execCtx.Err())
	case r := <-ch:
		if r.err != nil {
			return "", fmt.Errorf("rcon exec: %w", r.err)
		}
		return r.reply, nil
	}
}

func (c *rconClient) Close() error {
	log.Printf("Closing RCON connection")
	return c.conn.Close()
}
