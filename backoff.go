package supervisor

import "time"

// BackoffStrategy returns the delay before a retry attempt.
type BackoffStrategy interface {
	Next(retry int) time.Duration
}

// ExponentialBackoff grows retry delays exponentially up to Max.
type ExponentialBackoff struct {
	Min, Max time.Duration
	Factor   float64
}

// Next returns the delay for retry.
func (b ExponentialBackoff) Next(retry int) time.Duration {
	if b.Min <= 0 {
		return 0
	}
	if b.Factor <= 0 {
		b.Factor = 1
	}

	d := float64(b.Min)

	for i := 0; i < retry; i++ {
		d *= b.Factor
		if b.Max > 0 && time.Duration(d) > b.Max {
			return b.Max
		}
	}

	return time.Duration(d)
}
