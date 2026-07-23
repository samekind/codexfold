package reconcile

import (
	"errors"
	"math"
)

func estimatedOutputBytes(sourceBytes int64) (int64, error) {
	const fixedOverhead = int64(1 << 20)
	if sourceBytes < 0 {
		return 0, errors.New("output byte estimate cannot be negative")
	}
	overhead := sourceBytes/16 + fixedOverhead
	if sourceBytes > math.MaxInt64-overhead {
		return 0, errors.New("output byte estimate overflow")
	}
	return sourceBytes + overhead, nil
}
