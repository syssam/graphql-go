package loader

import (
	"context"
	"testing"
	"time"
)

// A batch serves every Load waiting on it, so one caller giving up must not
// cancel it for the rest; only all of them giving up does.
func TestBatchContextIsCancelledOnlyWhenEveryWaiterIs(t *testing.T) {
	a, cancelA := context.WithCancel(context.Background())
	b, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	waiters := map[int][]*waiter[int]{1: {{ctx: a}}, 2: {{ctx: b}, {ctx: a}}}
	ctx, release := batchContext([]int{1, 2}, waiters)
	defer release()

	cancelA()
	select {
	case <-ctx.Done():
		t.Fatal("batch cancelled while a waiter was still live")
	case <-time.After(20 * time.Millisecond):
	}
	cancelB()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("batch not cancelled after every waiter gave up")
	}
}

func TestBatchContextDeadlineIsTheLatestWaiters(t *testing.T) {
	now := time.Now()
	early, c1 := context.WithDeadline(context.Background(), now.Add(time.Minute))
	defer c1()
	late, c2 := context.WithDeadline(context.Background(), now.Add(time.Hour))
	defer c2()
	ctx, release := batchContext([]int{1}, map[int][]*waiter[int]{1: {{ctx: early}, {ctx: late}}})
	defer release()
	if dl, ok := ctx.Deadline(); !ok || !dl.Equal(now.Add(time.Hour)) {
		t.Errorf("deadline = %v, %v; want the latest, %v", dl, ok, now.Add(time.Hour))
	}

	open, release2 := batchContext([]int{1}, map[int][]*waiter[int]{1: {{ctx: early}, {ctx: context.Background()}}})
	defer release2()
	if _, ok := open.Deadline(); ok {
		t.Error("a batch with a waiter that has no deadline was given one")
	}
}
