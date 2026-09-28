package model

import (
	"sync"
	"sync/atomic"
)

type SafeCounter struct {
	wg    sync.WaitGroup
	count atomic.Int64
}

func (sc *SafeCounter) Add(delta int) {
	sc.wg.Add(delta)
	sc.count.Add(int64(delta))
}

func (sc *SafeCounter) Done() {
	sc.wg.Done()
	sc.count.Add(-1)
}

func (sc *SafeCounter) Wait() {
	sc.wg.Wait()
}

func (sc *SafeCounter) Count() int64 {
	return sc.count.Load()
}

func (sc *SafeCounter) HasCount() bool {
	return sc.count.Load() > 0
}
