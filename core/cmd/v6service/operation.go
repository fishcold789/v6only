package main

import (
	"errors"
	"sync"
)

var errBusy = errors.New("SERVICE_BUSY: 网络配置操作正在执行，请稍后重试")

// Reject contention promptly instead of spending a client's deadline in a queue.
func withOperation(lock *sync.Mutex, run func() (any, error)) (any, error) {
	if !lock.TryLock() {
		return nil, errBusy
	}
	defer lock.Unlock()
	return run()
}
