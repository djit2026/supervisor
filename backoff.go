package supervisor

import "time"

type BackoffStrategy interface {
	Next(retry int) time.Duration
}

type ExponentialBackoff struct {
	Min, Max time.Duration
	Factor   float64
}

func (b ExponentialBackoff) Next(retry int) time.Duration {
	d := float64(b.Min)

	for i := 0; i < retry; i++ {
		d *= b.Factor
		if time.Duration(d) > b.Max {
			return b.Max
		}
	}

	return time.Duration(d)
}