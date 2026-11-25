package worker

// Queue triggers: messages of a queue with a function trigger are handed to the
// function in batches (an SQS-style event), at most the function's concurrency at
// once. When the invocation succeeds its messages are deleted, except those the
// function reports in batchItemFailures; when it fails they come back after the
// visibility timeout, and move to the dead-letter queue after max_receives.

func (a *Agent) PokeQueues() {
	select {
	case a.QueueWake <- struct{}{}:
	default:
	}
}
