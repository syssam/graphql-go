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

// A type alias is the same type under another name.
type opCtx = graphql.OperationContext

func throughATypeAlias(oc *opCtx) {
	if _, ok := oc.Get("k"); ok {
		return
	}
	oc.Set("k", 1) // want `Get\("k"\) followed by Set on the same key`
}

// A request-scoped struct that embeds the context promotes the same methods.
type request struct{ *graphql.OperationContext }

func throughAnEmbeddingStruct(r request) {
	if _, ok := r.Get("k"); ok {
		return
	}
	r.Set("k", 1) // want `Get\("k"\) followed by Set on the same key`
}

func throughMethodValues(oc *graphql.OperationContext) {
	get, set := oc.Get, oc.Set
	if _, ok := get("k"); ok {
		return
	}
	set("k", 1) // want `get\("k"\) followed by Set on the same key`
}

func throughMethodExpressions(oc *graphql.OperationContext) {
	if _, ok := (*graphql.OperationContext).Get(oc, keyA{}); ok {
		return
	}
	(*graphql.OperationContext).Set(oc, keyA{}, 1) // want `Get\(oc, keyA{}\) followed by Set on the same key`
}

// The first argument of a method expression is the context, not the key.
func methodExpressionsOnTwoKeys(oc *graphql.OperationContext) {
	(*graphql.OperationContext).Get(oc, keyA{})
	(*graphql.OperationContext).Set(oc, keyB{}, 1)
}

// The context behind an interface it was assigned to in the same function.
type store interface {
	Get(key any) (any, bool)
	Set(key, value any)
}

func throughAnInterface(oc *graphql.OperationContext) {
	var s store = oc
	if _, ok := s.Get("k"); ok {
		return
	}
	s.Set("k", 1) // want `Get\("k"\) followed by Set on the same key`
}

// An interface parameter may hold anything, so nothing is known about it.
func throughAnInterfaceParameter(s store) {
	if _, ok := s.Get("k"); ok {
		return
	}
	s.Set("k", 1)
}

// One key spelled two ways: a variable holding the value, a package variable,
// a constant.
var sharedKey = keyA{}

const namedKey = "k"

func keyHeldInAVariable(oc *graphql.OperationContext) {
	if _, ok := oc.Get(keyA{}); ok {
		return
	}
	k := keyA{}
	oc.Set(k, 1) // want `Get\(keyA{}\) followed by Set on the same key`
}

func keyInAPackageVariable(oc *graphql.OperationContext) {
	if _, ok := oc.Get(sharedKey); ok {
		return
	}
	oc.Set(keyA{}, 1) // want `Get\(sharedKey\) followed by Set on the same key`
}

func keyAsAConstant(oc *graphql.OperationContext) {
	if _, ok := oc.Get(namedKey); ok {
		return
	}
	oc.Set("k", 1) // want `Get\(namedKey\) followed by Set on the same key`
}

// A string and a named string type with the same text are two keys, as they
// are to the map behind Get.
type textKey string

func sameTextTwoTypes(oc *graphql.OperationContext) {
	oc.Get(textKey("k"))
	oc.Set("k", 1)
}

// Two pointers are two keys, however alike the things they point at.
func twoPointerKeys(oc *graphql.OperationContext) {
	a, b := &keyA{}, &keyA{}
	oc.Get(a)
	oc.Set(b, 1)
}

func onePointerKey(oc *graphql.OperationContext) {
	a := &keyA{}
	b := a
	if _, ok := oc.Get(a); ok {
		return
	}
	oc.Set(b, 1) // want `Get\(a\) followed by Set on the same key`
}

// A variable written after it was declared holds whatever it was last given.
func reassignedKey(oc *graphql.OperationContext, other any) {
	var k any = keyA{}
	k = other
	oc.Get(keyA{})
	oc.Set(k, 1)
}

// So does one whose field was.
type fieldKey struct{ name string }

func keyWithAFieldWritten(oc *graphql.OperationContext) {
	k := fieldKey{}
	k.name = "other"
	oc.Get(fieldKey{})
	oc.Set(k, 1)
}

func keyWithFields(oc *graphql.OperationContext) {
	k := fieldKey{name: "a"}
	if _, ok := oc.Get(fieldKey{name: "a"}); ok {
		return
	}
	oc.Set(k, 1) // want `Get\(fieldKey{.*}\) followed by Set on the same key`
}

func keysWithDifferentFields(oc *graphql.OperationContext) {
	oc.Get(fieldKey{name: "a"})
	oc.Set(fieldKey{name: "b"}, 1)
}

// The installer is written above the check and runs under it.
func installerDeclaredFirst(oc *graphql.OperationContext) {
	install := func() {
		oc.Set("k", 1) // want `Get\("k"\) followed by Set on the same key`
	}
	if _, ok := oc.Get("k"); !ok {
		install()
	}
}

// The same closure called before the read is set-then-get.
func installerRunFirst(oc *graphql.OperationContext) any {
	install := func() { oc.Set("k", 1) }
	install()
	v, _ := oc.Get("k")
	return v
}

// A reader written above the write and called after it is set-then-get too.
func readerDeclaredFirst(oc *graphql.OperationContext) any {
	read := func() any { v, _ := oc.Get("k"); return v }
	oc.Set("k", 1)
	return read()
}

func getOrSet(oc *graphql.OperationContext) any {
	v, _ := oc.GetOrSet("k", 1)
	return v
}

// Another type with a Get and a Set is not the context.
type session struct{ values map[any]any }

func (s *session) Get(key any) (any, bool) { v, ok := s.values[key]; return v, ok }
func (s *session) Set(key, value any)      { s.values[key] = value }

func anotherTypesSet(oc *graphql.OperationContext, s *session) {
	if v, ok := oc.Get("k"); ok {
		s.Set("k", v)
	}
}

func anotherTypesPair(s *session) {
	if _, ok := s.Get("k"); ok {
		return
	}
	s.Set("k", 1)
}

// A function given two contexts carries state from one operation to another.
func betweenTwoContexts(from, to *graphql.OperationContext) {
	if v, ok := from.Get("k"); ok {
		to.Set("k", v)
	}
}
