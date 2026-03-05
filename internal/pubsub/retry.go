package pubsub

import "time"

// NextBackoff computes the next retry delay for a failed PubSub message.
func NextBackoff(policy RetryPolicy, attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	base := policy.Backoff
	if base <= 0 {
		base = 100 * time.Millisecond
	}
	d := base << (attempt - 1)
	if policy.MaxBackoff > 0 && d > policy.MaxBackoff {
		return policy.MaxBackoff
	}
	return d
}

// ShouldRetry reports whether a message should be retried for the given attempt count.
func ShouldRetry(policy RetryPolicy, attempts int) bool {
	if policy.MaxRetries <= 0 {
		return false
	}
	return attempts <= policy.MaxRetries
}
