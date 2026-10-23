package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestAccountOwnershipChecksRemainHeldUnderConcurrentMonitorAndGuards(t *testing.T) {
	db, _, _ := integrationStore(t)
	saveTestAccount(t, db, "concurrent-owner")
	ownerCtx, release, err := db.AcquireAccountContext(context.Background(), "concurrent-owner")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	lease, ok := ownerCtx.Value(leaseContextKey{}).(*accountLease)
	if !ok {
		t.Fatal("acquired context has no ownership lease")
	}

	// Each wave releases all callers together against the same real PostgreSQL
	// session. Both the monitor and write guards use this ownership check.
	const callers = 32
	const waves = 8
	failures := make(chan error, callers*waves)
	for range waves {
		start := make(chan struct{})
		var ready sync.WaitGroup
		var finished sync.WaitGroup
		ready.Add(callers)
		finished.Add(callers)
		for range callers {
			go func() {
				defer finished.Done()
				ready.Done()
				<-start
				held, err := lease.owns(ownerCtx)
				if err != nil {
					failures <- err
				} else if !held {
					failures <- fmt.Errorf("healthy session reported ownership lost")
				}
			}()
		}
		ready.Wait()
		close(start)
		finished.Wait()
	}
	close(failures)
	for failure := range failures {
		t.Errorf("concurrent ownership check: %v", failure)
	}
	if err := ownerCtx.Err(); err != nil {
		t.Fatalf("healthy concurrent checks cancelled the owner: %v", err)
	}
	_, competingRelease, err := db.AcquireAccountContext(context.Background(), "concurrent-owner")
	if competingRelease != nil {
		competingRelease()
	}
	if !errors.Is(err, ErrAccountLocked) {
		t.Fatalf("healthy owner did not exclude a competing executor: %v", err)
	}

	release()
	if !errors.Is(ownerCtx.Err(), context.Canceled) {
		t.Fatalf("explicit release did not cancel the ownership context: %v", ownerCtx.Err())
	}
	nextCtx, nextRelease, err := db.AcquireAccountContext(context.Background(), "concurrent-owner")
	if err != nil {
		t.Fatalf("released account could not be acquired by the next executor: %v", err)
	}
	defer nextRelease()
	nextLease, ok := nextCtx.Value(leaseContextKey{}).(*accountLease)
	if !ok {
		t.Fatal("successor context has no ownership lease")
	}
	if held, err := nextLease.owns(nextCtx); err != nil || !held {
		t.Fatalf("successor does not hold account and company ownership: held %v, error %v", held, err)
	}
}
