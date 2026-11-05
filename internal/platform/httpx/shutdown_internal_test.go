package httpx

import "github.com/is-koala/koala-api/internal/platform/config"

// Compile-time check: config.MinShutdownTimeout == MinShutdownTimeout.
// Converting a negative constant difference to uint64 does not compile, so
// the two constants below fail the build if either side is larger.
const (
	_ = uint64(config.MinShutdownTimeout - MinShutdownTimeout)
	_ = uint64(MinShutdownTimeout - config.MinShutdownTimeout)
)
