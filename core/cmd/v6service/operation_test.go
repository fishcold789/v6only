package main

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestBusyDoesNotQueue(t *testing.T) {
	var lock sync.Mutex
	lock.Lock()
	done := make(chan error, 1)
	go func() {
		_, err := withOperation(&lock, func() (any, error) { t.Error("busy operation executed"); return nil, nil })
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, errBusy) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("request queued behind a long-running operation")
	}
	lock.Unlock()
	_, err := withOperation(&lock, func() (any, error) { return nil, errors.New("failure") })
	if err == nil || !lock.TryLock() {
		t.Fatal("operation did not release its lock")
	}
	lock.Unlock()
}
