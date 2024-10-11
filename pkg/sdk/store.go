package sdk

import (
	"encoding/json"
	"fmt"
	"sync/atomic"

	"github.com/flagforge/flagforge/pkg/evaluation"
	"github.com/flagforge/flagforge/pkg/types"
)

// Store publishes immutable snapshots. Writers copy before publication; readers
// load one pointer for an entire evaluation, including prerequisites and segments.
type Store struct {
	current atomic.Pointer[types.Snapshot]
}

func (s *Store) Replace(snapshot types.Snapshot) (bool, error) {
	if err := evaluation.ValidateSnapshot(snapshot); err != nil {
		return false, fmt.Errorf("validate snapshot: %w", err)
	}
	b, err := json.Marshal(snapshot)
	if err != nil {
		return false, err
	}
	var owned types.Snapshot
	if err = json.Unmarshal(b, &owned); err != nil {
		return false, err
	}
	evaluation.Prepare(&owned)
	for {
		old := s.current.Load()
		if old != nil {
			if old.Project != owned.Project || old.Environment != owned.Environment {
				return false, fmt.Errorf("snapshot scope changed")
			}
			if owned.Version <= old.Version {
				return false, nil
			}
		}
		if s.current.CompareAndSwap(old, &owned) {
			return true, nil
		}
	}
}

func (s *Store) Version() int64 {
	if p := s.current.Load(); p != nil {
		return p.Version
	}
	return -1
}

// Snapshot returns an owned copy so callers cannot mutate published data.
func (s *Store) Snapshot() types.Snapshot {
	p := s.current.Load()
	if p == nil {
		return types.Snapshot{}
	}
	b, _ := json.Marshal(p)
	var out types.Snapshot
	_ = json.Unmarshal(b, &out)
	return out
}
