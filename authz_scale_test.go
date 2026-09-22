package graphql

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// The instance-authorization tests elsewhere in this package use three rows
// and a policy that is a pure function: no latency, no failure, and batching
// that costs nothing whether or not it happens. That is the wrong shape for
// the thing the design exists for. WithObjectAuthBatch defaults to 50 because
// that is OpenFGA's BatchCheck default, which is to say the whole per-list
// batching design assumes the policy is a network call.
//
// These tests give it that shape -- a backend with latency that can fail --
// and ask what a list of a thousand rows actually costs.

type scaleRow struct {
	ID    string
	Owner *scaleOwner
}

type scaleOwner struct{ ID string }

// guardedScaleSchema builds a schema with a guarded list type and a guarded
// single-object field beneath it, so one fixture covers both the batched path
// and the per-field one the godoc calls an N+1.
func guardedScaleSchema(tb testing.TB, rows int) (*Schema, []*scaleRow) {
	tb.Helper()
	const sdl = `
directive @authorizeObject on OBJECT
type Owner @authorizeObject { id: ID! }
type Row @authorizeObject { id: ID! owner: Owner }
type Query { rows: [Row!]! one: Row }
`
	data := make([]*scaleRow, rows)
	for i := range data {
		id := strconv.Itoa(i)
		data[i] = &scaleRow{ID: id, Owner: &scaleOwner{ID: "o" + id}}
	}
	s, err := NewSchema(SDL(sdl),
		Object[scaleOwner]("Owner",
			Field("id", func(o *scaleOwner) ID { return ID(o.ID) }),
		),
		Object[scaleRow]("Row",
			Field("id", func(r *scaleRow) ID { return ID(r.ID) }),
			Field("owner", func(r *scaleRow) *scaleOwner { return r.Owner }),
		),
		Query(
			Resolve("rows", func(context.Context, Root) ([]*scaleRow, error) { return data, nil }),
			Resolve("one", func(context.Context, Root) (*scaleRow, error) { return data[0], nil }),
		),
	)
	if err != nil {
		tb.Fatalf("NewSchema: %v", err)
	}
	return s, data
}

// remotePolicy stands in for a policy decision point reached over a network:
// it counts what it was asked, can take time to answer, and can fail.
type remotePolicy struct {
	calls  atomic.Int64
	checks atomic.Int64

	latency time.Duration
	// failAfter makes the call that many calls in fail, so a failure can be
	// placed in the middle of a run rather than only at its start.
	failAfter int64
	// widest records the largest single call, which is what says whether the
	// batch size is doing anything.
	widest atomic.Int64
}

func (p *remotePolicy) AuthorizeObjects(_ context.Context, checks []ObjectCheck) ([]Outcome, error) {
	n := p.calls.Add(1)
	p.checks.Add(int64(len(checks)))
	for {
		w := p.widest.Load()
		if int64(len(checks)) <= w || p.widest.CompareAndSwap(w, int64(len(checks))) {
			break
		}
	}
	if p.latency > 0 {
		time.Sleep(p.latency)
	}
	if p.failAfter > 0 && n >= p.failAfter {
		return nil, errors.New("policy backend unavailable")
	}
	return make([]Outcome, len(checks)), nil
}

// A thousand rows must not be a thousand round trips. The numbers are exact,
// not approximate: ceil(n/batch) calls carrying n checks in total, none of
// them wider than the batch.
func TestObjectAuthBatchingAtScale(t *testing.T) {
	const rows = 1000
	s, _ := guardedScaleSchema(t, rows)

	for _, batch := range []int{50, 200, 1000} {
		t.Run(fmt.Sprintf("batch%d", batch), func(t *testing.T) {
			p := &remotePolicy{}
			e := NewExecutor(s, WithObjectAuthorizer(p), WithObjectAuthBatch(batch))
			if resp := run(t, e, `{ rows { id } }`, ""); len(resp.Errors) > 0 {
				t.Fatalf("unexpected error: %v", resp.Errors[0])
			}

			wantCalls := int64((rows + batch - 1) / batch)
			if got := p.calls.Load(); got != wantCalls {
				t.Errorf("calls = %d, want %d for %d rows at a batch of %d", got, wantCalls, rows, batch)
			}
			if got := p.checks.Load(); got != rows {
				t.Errorf("checks = %d, want one per row (%d)", got, rows)
			}
			if got := p.widest.Load(); got > int64(batch) {
				t.Errorf("widest call carried %d checks, above the batch of %d", got, batch)
			}
		})
	}
}

// The finding worth writing down: batches are issued one after another, not
// concurrently, so the policy's latency multiplies by the number of batches
// rather than being paid once. A thousand rows at the default batch of 50 is
// twenty sequential round trips -- at a modest 5ms that is 100ms added to a
// single field, and halving the batch size doubles it.
//
// Run under synctest, so the time is virtual and the assertion is exact
// rather than a tolerance around a real clock.
func TestObjectAuthBatchesAreSequential(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const (
			rows    = 1000
			batch   = 50
			latency = 5 * time.Millisecond
		)
		s, _ := guardedScaleSchema(t, rows)
		p := &remotePolicy{latency: latency}
		e := NewExecutor(s, WithObjectAuthorizer(p), WithObjectAuthBatch(batch))

		start := time.Now()
		if resp := run(t, e, `{ rows { id } }`, ""); len(resp.Errors) > 0 {
			t.Fatalf("unexpected error: %v", resp.Errors[0])
		}
		elapsed := time.Since(start)

		want := time.Duration(rows/batch) * latency
		t.Logf("%d rows at a batch of %d: %d calls, %v of policy latency", rows, batch, p.calls.Load(), elapsed)
		if elapsed != want {
			t.Errorf("the list took %v, want %v: %d batches of %v, paid one after another",
				elapsed, want, rows/batch, latency)
		}
		// The control. If the batches ran concurrently this would be the
		// answer instead, and the test above would be measuring nothing.
		if elapsed == latency {
			t.Error("the batches ran concurrently; this test no longer measures what it says")
		}
	})
}

// A guarded object behind a single-object field is one call carrying one
// check, which the godoc states rather than promises away. Quantified here so
// the cost is a number and not a caveat: a list of n rows each reaching one
// guarded child issues ceil(n/batch) calls for the list and n more beneath it.
func TestObjectAuthNestedSingleFieldIsOneCallPerRow(t *testing.T) {
	const rows = 100
	s, _ := guardedScaleSchema(t, rows)
	p := &remotePolicy{}
	e := NewExecutor(s, WithObjectAuthorizer(p), WithObjectAuthBatch(50))

	if resp := run(t, e, `{ rows { id owner { id } } }`, ""); len(resp.Errors) > 0 {
		t.Fatalf("unexpected error: %v", resp.Errors[0])
	}

	// 2 for the list of 100 at a batch of 50, then one per row for owner.
	const wantCalls = 2 + rows
	if got := p.calls.Load(); got != wantCalls {
		t.Errorf("calls = %d, want %d (%d batched for the list, %d single for owner)",
			got, wantCalls, 2, rows)
	}
	// Worth seeing next to it: every owner call carries exactly one check, so
	// the batch size cannot help here. Modelling the position as a list, or
	// caching inside the policy, is what does.
	if got := p.widest.Load(); got != 50 {
		t.Errorf("widest = %d, want 50 from the list; the owner calls carry one each", got)
	}
}

// A policy backend that fails partway through must not be the reason the rows
// it had not reached become visible. The failure is placed in the middle on
// purpose: failing on the first call proves much less, because there is
// nothing after it to leak.
func TestObjectAuthFailsClosedFromTheMiddleOfAList(t *testing.T) {
	const rows = 1000
	s, _ := guardedScaleSchema(t, rows)
	p := &remotePolicy{failAfter: 3} // the third of twenty batches
	e := NewExecutor(s, WithObjectAuthorizer(p), WithObjectAuthBatch(50))

	resp := run(t, e, `{ rows { id } }`, "")
	if len(resp.Errors) == 0 {
		t.Fatal("a failing policy backend produced no error")
	}
	if got := string(resp.Data); strings.Contains(got, `"id"`) {
		t.Fatalf("rows were written despite the policy failing: %.120s", got)
	}
	// It must also stop asking. Continuing through the remaining batches
	// would be seventeen more round trips to a backend already known to be
	// down, on every request, which is how a policy outage becomes an outage.
	if got := p.calls.Load(); got != 3 {
		t.Errorf("calls = %d, want 3: the run must stop at the first failure, not finish the list", got)
	}
}

// The consumer driving this library has 1 589 authorization sites. The
// existing wide benchmarks stop at 256, so the per-request cost of the
// Authorizer's walk and of the Decision it fills has never been measured
// anywhere near that. Allocation counts are the figure to read.
func BenchmarkAuthorizerWide1589(b *testing.B) { benchWideAuthz(b, 1589) }
