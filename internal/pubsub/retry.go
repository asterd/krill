package pubsub

import "time"

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

func ShouldRetry(policy RetryPolicy, attempts int) bool {
	if policy.MaxRetries <= 0 {
		return false
	}
	return attempts <= policy.MaxRetries
}
