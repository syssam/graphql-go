package schema

import (
	"context"
	"sync"

	"github.com/syssam/graphql-go/examples/blog/graph/model"
)

// broker fans each created post out to every open subscription.
//
// Two rules make the difference between a demo and something that survives a
// real client. A subscriber that is not keeping up is dropped rather than
// allowed to block: a mutation must never wait on a slow WebSocket, so the
// send is non-blocking and a full buffer loses the event. And a subscriber is
// removed when its context ends, which is how the transports report both an
// unsubscribe and a disconnect; without that the map grows for the life of the
// process.
type broker struct {
	mu   sync.Mutex
	next int
	subs map[int]chan *model.Post
}

// subscribe returns a channel of posts, closed when ctx ends.
func (b *broker) subscribe(ctx context.Context) <-chan *model.Post {
	ch := make(chan *model.Post, 16)

	b.mu.Lock()
	if b.subs == nil {
		b.subs = make(map[int]chan *model.Post)
	}
	id := b.next
	b.next++
	b.subs[id] = ch
	b.mu.Unlock()

	go func() {
		<-ctx.Done()
		b.remove(id)
	}()
	return ch
}

func (b *broker) remove(id int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	// Closing under the same lock publish holds is what makes a send to a
	// closed channel impossible.
	if ch, ok := b.subs[id]; ok {
		delete(b.subs, id)
		close(ch)
	}
}

func (b *broker) publish(p *model.Post) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
		select {
		case ch <- p:
		default:
		}
	}
}

// subscribers reports how many subscriptions are open, for tests.
func (b *broker) subscribers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}
