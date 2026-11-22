package media

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/riverqueue/river"
)

// WorkDeleteObjects runs the object deletion job whose River arguments are
// args through its registered worker, as River would: the tests that run
// the queued deletions go through deleteObjectsWorker.Work, so what the
// worker passes on (the Keep of ADR-0035 Karar 7) is under test too.
func (s *Service) WorkDeleteObjects(ctx context.Context, args []byte) error {
	var a deleteObjectsArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return fmt.Errorf("decode delete objects args: %w", err)
	}
	return (&deleteObjectsWorker{service: s}).Work(ctx, &river.Job[deleteObjectsArgs]{Args: a})
}
