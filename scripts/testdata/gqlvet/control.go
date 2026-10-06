// Package control is code gqlvet must report. It is the positive control for
// an analyzer whose usual answer is silence: one that has stopped matching
// the context type is silent too. CI runs gqlvet on this directory and
// requires the findings, and the lint module's own test loads it against the
// real graphql.OperationContext and requires one finding on each line marked
// "want". It is under testdata so that ./... never reaches it.
package control

import "github.com/syssam/graphql-go"

type stateKey struct{}

type state struct{}

func checkThenAct(oc *graphql.OperationContext) *state {
	if v, ok := oc.Get(stateKey{}); ok {
		return v.(*state)
	}
	s := &state{}
	oc.Set(stateKey{}, s) // want
	return s
}

type request struct{ *graphql.OperationContext }

func throughAnEmbeddingStruct(r request) {
	if _, ok := r.Get(stateKey{}); !ok {
		r.Set(stateKey{}, &state{}) // want
	}
}

func atomically(oc *graphql.OperationContext) *state {
	v, _ := oc.GetOrSet(stateKey{}, &state{})
	return v.(*state)
}
