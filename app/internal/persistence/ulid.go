package persistence

import (
	"crypto/rand"
	"fmt"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
)

// entropy is a monotonic ULID source, mirroring app/internal/eventlog's own
// newULID: for two ids minted in the same millisecond it strictly increases
// the random component, so task_instances.id sorts in mint order. It is
// stateful and not safe for concurrent use without entMu.
var (
	entMu   sync.Mutex
	entropy = ulid.Monotonic(rand.Reader, 0)
)

func newULID() string {
	entMu.Lock()
	defer entMu.Unlock()
	id, err := ulid.New(ulid.Timestamp(time.Now()), entropy)
	if err != nil {
		// crypto/rand essentially never fails; fall back to a time-only id.
		return fmt.Sprintf("t%020d", time.Now().UnixNano())
	}
	return id.String()
}
