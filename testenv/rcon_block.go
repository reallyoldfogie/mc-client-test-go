package testenv

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

type BlockResolver struct {
	Helper     RCONHelper
	Candidates []string

	mu    sync.RWMutex
	cache map[string]string // "x,y,z" -> id ("" = unknown)
}

func NewBlockResolver(helper RCONHelper, candidates []string) *BlockResolver {
	return &BlockResolver{
		Helper:     helper,
		Candidates: append([]string(nil), candidates...),
		cache:      make(map[string]string),
	}
}

// GetBlock probes each candidate with `execute if block x y z <id> run say <id>`.
// First match wins; results are cached per coordinate.
func (r *BlockResolver) GetBlock(ctx context.Context, x, y, z int) (string, error) {
	key := fmt.Sprintf("%d,%d,%d", x, y, z)

	r.mu.RLock()
	if id, ok := r.cache[key]; ok {
		if id == "" {
			r.mu.RUnlock()
			return "", fmt.Errorf("no matching block for %s", key)
		}
		r.mu.RUnlock()
		return id, nil
	}
	r.mu.RUnlock()

	if len(r.Candidates) == 0 {
		return "", fmt.Errorf("no block candidates configured")
	}

	for _, id := range r.Candidates {
		cmd := fmt.Sprintf(
			"execute if block %d %d %d %s run say %s",
			x, y, z, id, id,
		)
		reply, err := r.Helper.Exec(ctx, cmd)
		if err != nil {
			continue
		}
		if strings.Contains(reply, id) {
			r.mu.Lock()
			r.cache[key] = id
			r.mu.Unlock()
			return id, nil
		}
	}

	r.mu.Lock()
	r.cache[key] = ""
	r.mu.Unlock()
	return "", fmt.Errorf("no matching block for %s", key)
}
