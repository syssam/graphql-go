package storefront

import (
	"context"
	"strings"
	"testing"
	"time"

	graphql "github.com/syssam/graphql-go"
)

// event reads one response from the stream, or fails.
func event(t *testing.T, events <-chan *graphql.Response) *graphql.Response {
	t.Helper()
	select {
	case resp, ok := <-events:
		if !ok {
			t.Fatal("the stream closed while an event was expected")
		}
		return resp
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for an event")
		return nil
	}
}

// A subscription runs the whole operation chain per event, so the authorizer
// and the row check run again for every one. That is the property a stream
// has to have and a one-off check at open cannot give it: a customer
// subscribing to orderPlaced sees their own orders and stays unaware of
// everyone else's.
func TestSubscriptionAppliesTheRowCheckPerEvent(t *testing.T) {
	e, store := newExecutor(t)

	ctx, cancel := context.WithCancel(WithPrincipal(context.Background(), CustomerPrincipal("c1")))
	defer cancel()

	events, err := e.Subscribe(ctx, &graphql.Request{Query: `subscription { orderPlaced { reference } }`})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	if _, err := store.Place("c1", []OrderLine{{SKU: "KB-01", Quantity: 1, UnitPrice: 10_00}}); err != nil {
		t.Fatalf("place: %v", err)
	}
	resp := event(t, events)
	if len(resp.Errors) > 0 {
		t.Fatalf("the subscriber's own order errored: %v", resp.Errors[0])
	}
	if got := string(resp.Data); got != `{"orderPlaced":{"reference":"SO-0004"}}` {
		t.Fatalf("data = %s, want the customer's own order", got)
	}
	resp.Release()

	// Someone else's order. Subscription.orderPlaced is Order!, a single
	// non-null position, so the row check cannot drop it -- it refuses, and
	// the refusal nulls the only field in the payload.
	//
	// An event still arrives. That is the limit of instance authorization on
	// a stream and the reason this asserts it rather than asserting silence:
	// the contents are withheld and the timing is not, so a subscriber can
	// still count other people's orders. Withholding the event itself is the
	// source's job, and a deployment that cares filters in the resolver that
	// opens the stream.
	if _, err := store.Place("c2", []OrderLine{{SKU: "MN-27", Quantity: 1, UnitPrice: 20_00}}); err != nil {
		t.Fatalf("place: %v", err)
	}
	resp = event(t, events)
	if len(resp.Errors) == 0 {
		t.Fatalf("another customer's order was delivered in full: %s", resp.Data)
	}
	if got := string(resp.Data); got != `null` {
		t.Fatalf("data = %s, want null for a refused non-null payload", got)
	}
	resp.Release()
}

// An anonymous caller is refused when the stream opens, not on its first
// event. Where the refusal happens is the whole difference for a client: a
// stream that opens and then errors is one graphql-ws will hold open and a
// browser will keep reconnecting to, and every reconnection runs the policy
// again for an answer that cannot change.
//
// Asserted as the one outcome rather than accepting either. A test written to
// pass whether the refusal came at open or at the first event would agree with
// the code instead of pinning it, and this is exactly the behaviour that a
// later change could quietly move.
func TestSubscriptionRefusesAnAnonymousCallerAtOpen(t *testing.T) {
	e, _ := newExecutor(t)

	ctx, cancel := context.WithCancel(WithPrincipal(context.Background(), Anonymous()))
	defer cancel()

	_, err := e.Subscribe(ctx, &graphql.Request{Query: `subscription { orderPlaced { reference } }`})
	if err == nil {
		t.Fatal("the stream opened for an anonymous caller")
	}
	if !strings.Contains(err.Error(), "Subscription.orderPlaced") {
		t.Fatalf("the refusal does not name the position: %v", err)
	}
}

// The control: the checks above withhold events, and a stream that simply
// never delivered anything would pass every one of them.
func TestSubscriptionDeliversToItsOwner(t *testing.T) {
	e, store := newExecutor(t)

	ctx, cancel := context.WithCancel(WithPrincipal(context.Background(), Staff("s1")))
	defer cancel()

	events, err := e.Subscribe(ctx, &graphql.Request{Query: `subscription { orderPlaced { reference } }`})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if _, err := store.Place("c2", []OrderLine{{SKU: "MN-27", Quantity: 1, UnitPrice: 20_00}}); err != nil {
		t.Fatalf("place: %v", err)
	}
	resp := event(t, events)
	if len(resp.Errors) > 0 {
		t.Fatalf("staff were refused their own event: %v", resp.Errors[0])
	}
	resp.Release()
}
