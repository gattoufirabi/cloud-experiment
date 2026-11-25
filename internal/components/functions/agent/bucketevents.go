package agent

import (
	"time"
)

func (a *Worker) refreshBucketTriggers() {
	a.BucketTrig.Mu.Lock()
	a.BucketTrig.At = time.Time{}
	a.BucketTrig.Mu.Unlock()
}
