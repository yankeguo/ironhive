package ironhive

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestRenewConcurrent exercises concurrent Renew calls racing with
// LeaseDeadline reads; it only proves anything under -race.
func TestRenewConcurrent(t *testing.T) {
	srv := fakeController(t)
	defer srv.Close()
	c := NewClient(srv.URL)
	ctx := context.Background()

	sb, err := c.Allocate(ctx, "default", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}

	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	errZeroDeadline := errors.New("zero lease deadline")
	for range workers {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range 50 {
				if err := sb.Renew(ctx, time.Hour); err != nil {
					errs <- err
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			for range 50 {
				if sb.LeaseDeadline().IsZero() {
					errs <- errZeroDeadline
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if sb.LeaseDeadline().IsZero() {
		t.Fatal("lease deadline missing after concurrent renews")
	}
}
