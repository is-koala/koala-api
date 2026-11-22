package media

import (
	"testing"

	"github.com/riverqueue/river"
)

// The completion job decodes images on a queue of its own (ADR-0035
// Karar 2); the object deletion and the sweep are I/O and stay on the
// default queue with the mail and code jobs. The completion is unique by
// the media's id alone, so a second request queues nothing while a job
// waits or runs.
func TestJobQueues(t *testing.T) {
	t.Parallel()

	if got := (completeArgs{}).InsertOpts(); got.Queue != Queue || !got.UniqueOpts.ByArgs {
		t.Errorf("completeArgs.InsertOpts() = %+v, want queue %q and unique by args", got, Queue)
	}
	if got := (deleteObjectsArgs{}).InsertOpts().Queue; got != "" && got != river.QueueDefault {
		t.Errorf("deleteObjectsArgs runs on %q, want the default queue", got)
	}
	if QueueWorkers < 1 {
		t.Errorf("QueueWorkers = %d; the media queue would never run", QueueWorkers)
	}
}
