package main

import "testing"

// The ordering IS the fix, so it gets a test rather than relying on two lines
// staying in the right order. Before this, boundedGC ran first and put a
// `docker buildx prune` between "the image is pushed" and "anyone is told" —
// 155.7 seconds of it on the frigate 0.19 build.
func TestFinishBuildEmitsBeforeHousekeeping(t *testing.T) {
	var order []string
	onChange := func(e JobEvent) { order = append(order, "event:"+string(e)) }
	gc := func() { order = append(order, "gc") }

	finishBuild(BuildCompleted, onChange, gc)

	if len(order) != 2 {
		t.Fatalf("expected an event and a gc, got %v", order)
	}
	if order[0] != "event:"+string(EventCompleted) {
		t.Fatalf("the terminal event must be emitted FIRST; order = %v", order)
	}
	if order[1] != "gc" {
		t.Fatalf("housekeeping must run after the event; order = %v", order)
	}
}

// Housekeeping is for a build that produced something. A cancelled or failed
// build has no new cache worth pruning on its behalf, and pruning there would
// charge the next build for this one's failure.
func TestFinishBuildSkipsHousekeepingOnNonCompletion(t *testing.T) {
	for _, tc := range []struct {
		state BuildState
		want  JobEvent
	}{
		{BuildCancelled, EventCancelled},
		{BuildFailed, EventFailed},
	} {
		var events []JobEvent
		gcRan := false
		finishBuild(tc.state, func(e JobEvent) { events = append(events, e) }, func() { gcRan = true })

		if len(events) != 1 || events[0] != tc.want {
			t.Fatalf("state %v: emitted %v, want [%v]", tc.state, events, tc.want)
		}
		if gcRan {
			t.Fatalf("state %v: housekeeping ran for a build that did not complete", tc.state)
		}
	}
}

// Negative control. Both tests above pass against a finishBuild that emits the
// event and never calls gc at all — which would silently drop the prune and let
// the builder's disk grow without bound. This pins that gc genuinely runs on the
// completed path.
func TestFinishBuildStillRunsHousekeepingOnCompletion(t *testing.T) {
	gcRan := false
	finishBuild(BuildCompleted, func(JobEvent) {}, func() { gcRan = true })
	if !gcRan {
		t.Fatal("housekeeping was dropped on the completed path")
	}
}
