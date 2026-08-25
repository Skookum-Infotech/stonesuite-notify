package workers

import (
	"context"
	"log"
	"time"
)

const (
	retryPollInterval        = 15 * time.Second
	staleProcessingThreshold = 5 * time.Minute
)

// RetryWorker polls for deliveries due for a retry (status='retrying' past
// its backoff) and also recovers rows stuck in 'processing' — e.g. a crash
// mid-send — treating them as a failed attempt so they don't stall
// forever.
type RetryWorker struct {
	Deps Deps
}

// Run blocks, polling on a ticker until ctx is cancelled.
func (w RetryWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(retryPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.pollOnce(ctx)
		}
	}
}

func (w RetryWorker) pollOnce(ctx context.Context) {
	claimed, err := w.Deps.Deliveries.ClaimDue(ctx, deliveryBatchSize, staleProcessingThreshold)
	if err != nil {
		log.Printf("workers: retry worker claim: %v", err)
		return
	}
	for _, d := range claimed {
		attemptDelivery(ctx, w.Deps, d)
	}
}
