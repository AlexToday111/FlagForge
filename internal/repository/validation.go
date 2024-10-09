package repository

import (
	"errors"
	"fmt"
	"github.com/flagforge/flagforge/pkg/evaluation"
	"github.com/flagforge/flagforge/pkg/types"
)

func validate(s types.Snapshot) error {
	if err := evaluation.ValidateSnapshot(s); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return nil
}
func isInvalid(err error) bool { return errors.Is(err, ErrInvalid) }
