package testenv

import (
	"sync"
	"testing"
)

// TestExecMutexForReturnsSameMutexForSameAddress confirms the core
// invariant Exec relies on to serialize RCON commands across every
// connection to the same server: two lookups for the same address must
// return the identical *sync.Mutex instance, not two independently
// lockable ones.
func TestExecMutexForReturnsSameMutexForSameAddress(t *testing.T) {
	a := execMutexFor("127.0.0.1:25575")
	b := execMutexFor("127.0.0.1:25575")
	if a != b {
		t.Fatal("execMutexFor returned different mutexes for the same address")
	}
}

// TestExecMutexForReturnsDistinctMutexesForDifferentAddresses confirms
// unrelated servers a process talks to don't serialize against each
// other.
func TestExecMutexForReturnsDistinctMutexesForDifferentAddresses(t *testing.T) {
	a := execMutexFor("127.0.0.1:25575")
	b := execMutexFor("127.0.0.1:25576")
	if a == b {
		t.Fatal("execMutexFor returned the same mutex for two different addresses")
	}
}

// TestExecMutexForIsSafeForConcurrentFirstUse exercises the actual
// failure mode this whole mechanism protects against: multiple
// goroutines racing to look up (and, for some, create) the mutex for
// the same never-before-seen address must all converge on one shared
// instance, with -race enabled to catch any data race in
// execMuMu/execMu themselves.
func TestExecMutexForIsSafeForConcurrentFirstUse(t *testing.T) {
	const addr = "127.0.0.1:25577"
	const goroutines = 32

	results := make(chan *sync.Mutex, goroutines)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- execMutexFor(addr)
		}()
	}
	wg.Wait()
	close(results)

	first := <-results
	for mu := range results {
		if mu != first {
			t.Fatal("concurrent first-use lookups for the same address returned different mutexes")
		}
	}
}
