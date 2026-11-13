package main

import "context"

// buildDeadline bounds ONE admitted service's build by BUILD_TIMEOUT. It is applied after admission
// so queueing behind other jobs never spends it. BuildTimeout <= 0 means no deadline.
func (e *engine) buildDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if e.cfg.BuildTimeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, e.cfg.BuildTimeout)
}
