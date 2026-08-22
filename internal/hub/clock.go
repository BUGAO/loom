package hub

import (
	"context"
	"fmt"
	"time"

	"loom/internal/model"
)

// The run's wall clock.
//
// RunTimeoutSec bounds one activation of a dynamic run — the last guarantee
// of termination once the DAG's acyclicity is gone. Two things it must NOT
// do, both learned from a real run:
//
//  1. Count time spent waiting on a human. A plan that sat 36 minutes in
//     "awaiting approval" burned 36 minutes of a 4-hour budget before any
//     work started. The clock below is a WORK clock: it pauses while the run
//     is parked on the approval gate or on ask_user.
//
//  2. Kill in-flight work at the deadline. Two tasks twenty minutes into
//     their sessions were canceled "because nobody remained to consume the
//     result" — when the thing that actually ran out was the permission to
//     START work. So the deadline is SOFT: delegation closes, the coordinator
//     is told to land what is in flight and deliver a verdict, and only a
//     grace period after that (bounded by the task timeout, so it cannot
//     stretch) becomes the hard stop.
//
// Before the deadline, a checkpoint notice gives the coordinator time to
// decide what still fits rather than discovering the wall by walking into it.

// Package variables rather than constants so tests can shrink them.
var (
	// clockTick is the work clock's resolution.
	clockTick = time.Second
	// checkpointMin is the least lead time the checkpoint notice gets.
	checkpointMin = 30 * time.Minute
	// deadlineIdleGrace is how long a soft-expired run with nothing in
	// flight waits for the coordinator to finish before the hard stop.
	deadlineIdleGrace = 15 * time.Minute
)

// checkpointLead is how long before the deadline the checkpoint notice
// fires: 10% of the budget, at least checkpointMin, never past the midpoint.
func checkpointLead(timeout time.Duration) time.Duration {
	lead := timeout / 10
	if lead < checkpointMin {
		lead = checkpointMin
	}
	if lead > timeout/2 {
		lead = timeout / 2
	}
	return lead
}

// hardGrace is the most a soft-expired run may keep running: every in-flight
// task is itself bounded by the task timeout, plus the idle grace for the
// coordinator's final rounds.
func (rs *RunSession) hardGrace() time.Duration {
	return time.Duration(rs.budget.TaskTimeoutSec)*time.Second + deadlineIdleGrace
}

// waitingOnHumanLocked reports whether the run is parked on a person — the
// approval gate or ask_user — which is the one kind of time that is not the
// run's to spend. Caller holds rs.mu.
func (rs *RunSession) waitingOnHumanLocked() bool {
	return rs.approvalPending ||
		(rs.run.Coordinator != nil && rs.run.Coordinator.Status == "awaiting_user")
}

// WaitingOnHuman is the exported view of waitingOnHumanLocked.
func (rs *RunSession) WaitingOnHuman() bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.waitingOnHumanLocked()
}

// WorkClock is the working time this activation has consumed: wall time
// minus time parked on a human.
func (rs *RunSession) WorkClock() time.Duration {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.workClock
}

// DeadlineReached reports whether the soft deadline has passed: no new task
// may start, in-flight ones finish, the coordinator owes a verdict.
func (rs *RunSession) DeadlineReached() bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.deadlineReached
}

// inFlightLocked counts tasks that are still running or parked on a
// question. Caller holds rs.mu.
func (rs *RunSession) inFlightLocked() int {
	n := 0
	for _, t := range rs.run.Tasks {
		if t.Status == model.TaskWorking || t.Status == model.TaskInputRequired || t.Status == model.TaskSubmitted {
			n++
		}
	}
	return n
}

// watchClock drives the work clock: accrues working time, fires the
// checkpoint, closes delegation at the soft deadline, and calls cfg.OnExpire
// at the hard one. It exits with the run.
func (rs *RunSession) watchClock(ctx context.Context) {
	timeout := time.Duration(rs.budget.RunTimeoutSec) * time.Second
	if timeout <= 0 {
		return
	}
	lead := checkpointLead(timeout)
	tick := time.NewTicker(clockTick)
	defer tick.Stop()
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-rs.finished:
			return
		case now := <-tick.C:
			elapsed := now.Sub(last)
			last = now

			rs.mu.Lock()
			if rs.closed || rs.verdict != nil {
				rs.mu.Unlock()
				return
			}
			if !rs.waitingOnHumanLocked() {
				rs.workClock += elapsed
			}
			remaining := timeout - rs.workClock

			// Checkpoint: one notice, once, with the lead time in it.
			if !rs.checkpointSent && remaining <= lead && !rs.deadlineReached {
				rs.checkpointSent = true
				rs.pendingNotice = joinNotice(rs.pendingNotice, fmt.Sprintf(
					"SYSTEM CHECKPOINT: about %s of this run's %s working-time budget remain (time parked on the user "+
						"does not count). When it runs out, delegation closes and in-flight tasks are allowed to finish, "+
						"then you must deliver finish_run. Plan for that now: do not start work that cannot land in time, "+
						"record_note where things stand, and make sure every in-flight task is one whose result you will "+
						"actually use.", remaining.Round(time.Minute), timeout.Round(time.Minute)))
				rs.appendEventLocked("checkpoint", "", fmt.Sprintf("wall clock checkpoint: %s of working time left", remaining.Round(time.Minute)))
				rs.notifyLocked()
				rs.mu.Unlock()
				continue
			}

			// Soft deadline: delegation closes; everything running finishes.
			if !rs.deadlineReached && remaining <= 0 {
				rs.deadlineReached = true
				rs.deadlineAt = now
				inflight := rs.inFlightLocked()
				rs.pendingNotice = joinNotice(rs.pendingNotice, fmt.Sprintf(
					"SYSTEM: the run's %s working-time budget is used up. No new task can be created from now on "+
						"(delegate will refuse). %d task(s) still in flight will be allowed to finish; wait for them, then "+
						"call finish_run with an honest verdict: what was achieved, verified how, and what remains for a "+
						"follow-up session. Do not wait for anything else.", timeout.Round(time.Minute), inflight))
				rs.appendEventLocked("run_status", "", fmt.Sprintf("wall clock reached (%s of working time): delegation closed, %d in-flight task(s) finishing, coordinator owes a verdict", timeout.Round(time.Minute), inflight))
				rs.notifyLocked()
				rs.mu.Unlock()
				continue
			}

			// Hard deadline: nothing left to wait for, or the grace is gone.
			if rs.deadlineReached {
				sinceSoft := now.Sub(rs.deadlineAt)
				idle := rs.inFlightLocked() == 0 && now.Sub(rs.lastTransition) >= deadlineIdleGrace && sinceSoft >= deadlineIdleGrace
				if idle || sinceSoft >= rs.hardGrace() {
					why := "the grace period after the wall clock ran out"
					if idle {
						why = "nothing was in flight and the coordinator delivered no verdict within the grace period"
					}
					rs.appendEventLocked("run_status", "", "hard stop: "+why)
					expire := rs.cfg.OnExpire
					rs.mu.Unlock()
					if expire != nil {
						expire()
					}
					return
				}
			}
			rs.mu.Unlock()
		}
	}
}
