package storefront

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Money is an amount in minor units. Integer cents rather than a float
// because a float cannot hold 0.10 and an invoice has to add up.
type Money int64

// String renders the amount as a decimal, which is what the Money scalar
// writes and what its parser reads back.
func (m Money) String() string {
	neg := ""
	if m < 0 {
		neg, m = "-", -m
	}
	return fmt.Sprintf("%s%d.%02d", neg, m/100, m%100)
}

// ParseMoney reads what String writes.
func ParseMoney(s string) (Money, error) {
	whole, frac, ok := strings.Cut(s, ".")
	if !ok {
		frac = "0"
	}
	if len(frac) > 2 {
		return 0, fmt.Errorf("money %q has more than two decimal places", s)
	}
	for len(frac) < 2 {
		frac += "0"
	}
	neg := strings.HasPrefix(whole, "-")
	n, err := strconv.ParseInt(strings.TrimPrefix(whole, "-"), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("money %q: %w", s, err)
	}
	c, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("money %q: %w", s, err)
	}
	total := n*100 + c
	if neg {
		total = -total
	}
	return Money(total), nil
}

// OrderStatus is the lifecycle of an order.
type OrderStatus int

// The order statuses, in lifecycle order.
const (
	StatusPending OrderStatus = iota
	StatusPaid
	StatusShipped
	StatusRefunded
)

// Customer is a buyer.
type Customer struct {
	ID    string
	Name  string
	Email string
}

// OrderLine is one item on an order.
type OrderLine struct {
	SKU         string
	Description string
	Quantity    int
	UnitPrice   Money
}

// Order is a placed order. CustomerID is what the instance policy reads to
// decide who may see it, and it never leaves this package: the schema
// exposes the customer through a resolver, not the foreign key.
type Order struct {
	ID         string
	Reference  string
	CustomerID string
	Status     OrderStatus
	PlacedAt   time.Time
	Total      Money
	Margin     Money
	Lines      []OrderLine
}

// Store is an in-memory stand-in for the database. Every method is safe for
// concurrent use because queries, mutations and subscriptions run against it
// at the same time.
//
// It answers what it is asked and enforces nothing: authorization lives in
// authz.go, where the schema can see it. A store that filtered by principal
// would hide the point of the example and would be the wrong place for the
// rule besides -- one forgotten call site and the filter is gone.
type Store struct {
	mu        sync.RWMutex
	customers map[string]*Customer
	orders    []*Order
	nextRef   int

	// customerFetches and customerKeys count what the DataLoader asked for.
	// A loader is only ever claimed to batch; these are how the example
	// proves it, and they are the same two numbers a real deployment would
	// alert on -- one key per fetch means batching has degraded to N+1.
	customerFetches atomic.Int64
	customerKeys    atomic.Int64

	placed broker
}

// CustomerFetches returns how many times the customer batch ran and how many
// keys it carried in total.
func (s *Store) CustomerFetches() (fetches, keys int64) {
	return s.customerFetches.Load(), s.customerKeys.Load()
}

// NewStore returns the store seeded with the example data.
func NewStore() *Store {
	s := &Store{
		customers: map[string]*Customer{
			"c1": {ID: "c1", Name: "Ada Lovelace", Email: "ada@example.com"},
			"c2": {ID: "c2", Name: "Grace Hopper", Email: "grace@example.com"},
		},
		nextRef: 3,
	}
	base := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	s.orders = []*Order{
		{
			ID: "o1", Reference: "SO-0001", CustomerID: "c1", Status: StatusShipped,
			PlacedAt: base, Total: 12_99, Margin: 4_20,
			Lines: []OrderLine{{SKU: "KB-01", Description: "Keyboard", Quantity: 1, UnitPrice: 12_99}},
		},
		{
			ID: "o2", Reference: "SO-0002", CustomerID: "c2", Status: StatusPaid,
			PlacedAt: base.Add(time.Hour), Total: 249_00, Margin: 90_00,
			Lines: []OrderLine{{SKU: "MN-27", Description: "Monitor", Quantity: 1, UnitPrice: 249_00}},
		},
		{
			ID: "o3", Reference: "SO-0003", CustomerID: "c1", Status: StatusPending,
			PlacedAt: base.Add(2 * time.Hour), Total: 35_50, Margin: 11_00,
			Lines: []OrderLine{{SKU: "CB-02", Description: "Cable", Quantity: 2, UnitPrice: 17_75}},
		},
	}
	return s
}

// OrderFilter is the decoded form of the OrderWhere input.
type OrderFilter struct {
	Status    *OrderStatus
	MinMargin *Money
}

// Orders returns up to first orders matching the filter, newest first.
func (s *Store) Orders(f *OrderFilter, first int) []*Order {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Order, 0, len(s.orders))
	for _, o := range s.orders {
		if f != nil {
			if f.Status != nil && o.Status != *f.Status {
				continue
			}
			if f.MinMargin != nil && o.Margin < *f.MinMargin {
				continue
			}
		}
		out = append(out, o)
	}
	slices.SortFunc(out, func(a, b *Order) int { return b.PlacedAt.Compare(a.PlacedAt) })
	if first > 0 && len(out) > first {
		out = out[:first]
	}
	return out
}

// Order returns one order, or nil.
func (s *Store) Order(id string) *Order {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, o := range s.orders {
		if o.ID == id {
			return o
		}
	}
	return nil
}

// OrdersOf returns every order belonging to one customer, newest first.
func (s *Store) OrdersOf(customerID string) []*Order {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Order, 0, 4)
	for _, o := range s.orders {
		if o.CustomerID == customerID {
			out = append(out, o)
		}
	}
	slices.SortFunc(out, func(a, b *Order) int { return b.PlacedAt.Compare(a.PlacedAt) })
	return out
}

// Customers returns the named customers in the order asked for, with a nil
// in the place of any that does not exist. The signature is the one a
// DataLoader batch function wants: one round trip for the whole wave.
func (s *Store) Customers(ids []string) []*Customer {
	s.customerFetches.Add(1)
	s.customerKeys.Add(int64(len(ids)))
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Customer, len(ids))
	for i, id := range ids {
		out[i] = s.customers[id]
	}
	return out
}

// Refund marks an order refunded and returns it.
func (s *Store) Refund(id string) (*Order, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, o := range s.orders {
		if o.ID != id {
			continue
		}
		if o.Status == StatusRefunded {
			return nil, fmt.Errorf("order %s is already refunded", id)
		}
		o.Status = StatusRefunded
		return o, nil
	}
	return nil, fmt.Errorf("order %s does not exist", id)
}

// Place records a new order and publishes it to every open subscription.
func (s *Store) Place(customerID string, lines []OrderLine) (*Order, error) {
	s.mu.Lock()
	if s.customers[customerID] == nil {
		s.mu.Unlock()
		return nil, fmt.Errorf("customer %s does not exist", customerID)
	}
	var total Money
	for _, l := range lines {
		total += l.UnitPrice * Money(l.Quantity)
	}
	o := &Order{
		ID:         "o" + strconv.Itoa(len(s.orders)+1),
		Reference:  fmt.Sprintf("SO-%04d", s.nextRef+1),
		CustomerID: customerID,
		Status:     StatusPending,
		PlacedAt:   time.Now().UTC(),
		Total:      total,
		Margin:     total / 3,
		Lines:      lines,
	}
	s.nextRef++
	s.orders = append(s.orders, o)
	s.mu.Unlock()

	// Published outside the lock: a slow subscriber must not hold up the
	// write that produced the event.
	s.placed.publish(o)
	return o, nil
}

// OrdersPlaced returns a stream of orders placed from now on. The stream
// ends when ctx ends, which is how both an unsubscribe and a disconnect
// arrive from every transport.
func (s *Store) OrdersPlaced(ctx context.Context) <-chan *Order {
	return s.placed.subscribe(ctx)
}

// broker fans each placed order out to every open subscription. Two rules,
// the same ones examples/blog uses: the send is non-blocking, so a
// subscriber that is not keeping up loses events rather than blocking the
// mutation that produced them, and a subscriber is removed when its context
// ends.
type broker struct {
	mu   sync.Mutex
	subs map[chan *Order]struct{}
}

func (b *broker) subscribe(ctx context.Context) <-chan *Order {
	ch := make(chan *Order, 8)
	b.mu.Lock()
	if b.subs == nil {
		b.subs = make(map[chan *Order]struct{})
	}
	b.subs[ch] = struct{}{}
	b.mu.Unlock()

	go func() {
		<-ctx.Done()
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
		close(ch)
	}()
	return ch
}

func (b *broker) publish(o *Order) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- o:
		default:
		}
	}
}
