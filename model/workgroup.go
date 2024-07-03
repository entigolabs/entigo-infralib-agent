package model

import (
	"sync"
	"sync/atomic"
)

type SafeCounter struct {
	wg    sync.WaitGroup
	count int64
}

func (sc *SafeCounter) Add(delta int) {
	sc.wg.Add(delta)
	atomic.AddInt64(&sc.count, int64(delta))
}

func (sc *SafeCounter) Done() {
	sc.wg.Done()
	atomic.AddInt64(&sc.count, -1)
}

func (sc *SafeCounter) Wait() {
	sc.wg.Wait()
}

func (sc *SafeCounter) Count() int64 {
	return atomic.LoadInt64(&sc.count)
}

func (sc *SafeCounter) HasCount() bool {
	return atomic.LoadInt64(&sc.count) > 0
}
