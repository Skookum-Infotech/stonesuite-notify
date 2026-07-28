package workers

import (
	"context"
	"log"
	"time"
)

const (
	consumerPollInterval = 2 * time.Second
	deliveryBatchSize    = 20
)

// QueueConsumer polls for newly-enqueued deliveries (status='pending') and
// attempts each one. It never touches retrying/stuck-processing rows —
// that is RetryWorker's job — so a slow provider can't starve new work
// from being picked up.
type QueueConsumer struct {
	Deps Deps
}

// Run blocks, polling on a ticker until ctx is cancelled.
func (c QueueConsumer) Run(ctx context.Context) {
	ticker := time.NewTicker(consumerPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.pollOnce(ctx)
		}
	}
}

func (c QueueConsumer) pollOnce(ctx context.Context) {
	claimed, err := c.Deps.Deliveries.ClaimPending(ctx, deliveryBatchSize)
	if err != nil {
		log.Printf("workers: queue consumer claim: %v", err)
		return
	}
	for _, d := range claimed {
		attemptDelivery(ctx, c.Deps, d)
	}
}
