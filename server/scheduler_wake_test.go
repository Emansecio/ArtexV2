package server

import "testing"

// Scheduler.Wake is the push path replacing the up-to-30s poll: it must be
// non-blocking and coalesce bursts into a single pending wake.
func TestSchedulerWakeCoalesces(t *testing.T) {
	sc := &Scheduler{wake: make(chan struct{}, 1)}

	select {
	case <-sc.wake:
		t.Fatal("wake channel should start empty")
	default:
	}

	sc.Wake()
	// A second and third push while a wake is pending must not block — they
	// fold into the same pending signal.
	sc.Wake()
	sc.Wake()

	select {
	case <-sc.wake:
	default:
		t.Fatal("wake should be pending after Wake()")
	}
	select {
	case <-sc.wake:
		t.Fatal("coalesced wakes must collapse into one pending signal")
	default:
	}
}

// Wake must be nil-safe: the Manager's schedWake hook can carry a nil receiver
// when the server never started a scheduler.
func TestSchedulerWakeNilSafe(t *testing.T) {
	var sc *Scheduler
	sc.Wake() // must not panic
}
