package b

import "github.com/syssam/graphql-go"

type keyA struct{}

type keyB struct{}

// The idiomatic context key is an empty struct, and two of them are two keys.
func distinctStructKeys(oc *graphql.OperationContext) {
	oc.Get(keyA{})
	oc.Set(keyB{}, 1)
}

func sameStructKey(oc *graphql.OperationContext) {
	if _, ok := oc.Get(keyA{}); ok {
		return
	}
	oc.Set(keyA{}, 1) // want `Get\(keyA{}\) followed by Set on the same key`
}

// The pair inside a closure is the closure's, and is reported once.
func inAClosure(oc *graphql.OperationContext) func() {
	return func() {
		if _, ok := oc.Get("k"); ok {
			return
		}
		oc.Set("k", 1) // want `Get\("k"\) followed by Set on the same key`
	}
}

// Parentheses do not make a different receiver.
func parenthesised(oc *graphql.OperationContext) {
	if _, ok := (oc).Get("k"); ok {
		return
	}
	oc.Set("k", 1) // want `Get\("k"\) followed by Set on the same key`
}

// The receiver is not part of the pattern. One operation has one context, and
// every spelling of it reaches the same state: an alias, a second call to
// OperationFrom, a field. Matching the receiver's text missed all of them.
func aliasedReceiver(oc *graphql.OperationContext) {
	c := oc
	if _, ok := oc.Get("k"); ok {
		return
	}
	c.Set("k", 1) // want `Get\("k"\) followed by Set on the same key`
}

type holder struct{ oc *graphql.OperationContext }

func throughAField(h holder, oc *graphql.OperationContext) {
	if _, ok := h.oc.Get("k"); ok {
		return
	}
	oc.Set("k", 1) // want `Get\("k"\) followed by Set on the same key`
}

// The check in the function and the act in a closure inside it are still one
// check-then-act: this is what a sync.Once or a goroutine around the Set
// looks like.
func setInAClosure(oc *graphql.OperationContext, do func(func())) {
	if _, ok := oc.Get("k"); !ok {
		do(func() {
			oc.Set("k", 1) // want `Get\("k"\) followed by Set on the same key`
		})
	}
}
