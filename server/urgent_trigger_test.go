package server

import (
	"testing"
	"time"
)

// hasUrgentTrigger is the gate that lets high-priority wakes (finding, hint,
// goal changes, cancellations) bypass quiet-mode skipping and the routine
// debounce. Anything other than a routine worker "done" must report urgent.
func TestHasUrgentTrigger(t *testing.T) {
	newTask := func() *Task { return &Task{ID: "t", notify: make(chan struct{}, 8)} }

	if (&Task{ID: "t", notify: make(chan struct{}, 8)}).hasUrgentTrigger() {
		t.Fatal("empty pending triggers must not be urgent")
	}

	task := newTask()
	task.NotifyDone(7)
	if task.hasUrgentTrigger() {
		t.Fatal("routine done trigger must not be urgent")
	}

	urgent := []struct {
		name string
		fire func(*Task)
	}{
		{"finding", func(t *Task) { t.NotifyFinding(7, "xss confirmed") }},
		{"hint", func(t *Task) { t.NotifyHint("human", []string{"check auth"}) }},
		{"goal", func(t *Task) { t.NotifyGoal([]string{"new goal"}) }},
		{"goal_edited", func(t *Task) { t.NotifyGoalEdited("old", "new") }},
		{"goal_deleted", func(t *Task) { t.NotifyGoalDeleted("gone") }},
		{"cancelled", func(t *Task) { t.NotifyCancelled(7, "sum", "reason") }},
	}
	for _, tc := range urgent {
		task := newTask()
		tc.fire(task)
		if !task.hasUrgentTrigger() {
			t.Fatalf("%s trigger must be urgent", tc.name)
		}
		// Drain clears the queue: afterwards nothing urgent remains.
		task.drainTriggers()
		if task.hasUrgentTrigger() {
			t.Fatalf("%s trigger still urgent after drain", tc.name)
		}
	}

	// A done burst with one urgent inside must still report urgent.
	task = newTask()
	task.NotifyDone(1)
	task.NotifyDone(2)
	task.NotifyFinding(3, "sqli")
	task.NotifyDone(4)
	if !task.hasUrgentTrigger() {
		t.Fatal("done+finding mix must be urgent")
	}
}

// plannerDebounceDuration: routine wakes coalesce at ~200ms; the env knob stays
// authoritative for tuning, and invalid values fall back to the default.
func TestPlannerDebounceDuration(t *testing.T) {
	t.Setenv("ARTEX_PLANNER_DEBOUNCE_MS", "")
	if got := plannerDebounceDuration(); got != 200*time.Millisecond {
		t.Fatalf("default debounce = %v, want 200ms", got)
	}
	t.Setenv("ARTEX_PLANNER_DEBOUNCE_MS", "450")
	if got := plannerDebounceDuration(); got != 450*time.Millisecond {
		t.Fatalf("env debounce = %v, want 450ms", got)
	}
	t.Setenv("ARTEX_PLANNER_DEBOUNCE_MS", "bogus")
	if got := plannerDebounceDuration(); got != 200*time.Millisecond {
		t.Fatalf("invalid env debounce = %v, want fallback 200ms", got)
	}
}
