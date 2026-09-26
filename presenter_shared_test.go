package graphql

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
)

// A masking presenter returning one shared *Error is the obvious way to write
// one. The executor writes the path and locations onto what it gets back, so
// without a copy concurrent requests race on that value (-race reports it)
// and the path one request set survives into the next.
func TestAPresenterMayReturnASharedError(t *testing.T) {
	masked := Errorf("internal error").WithCode(CodeInternal)
	_, e := newFixtureExecutor(t, WithErrorPresenter(func(context.Context, error) *Error { return masked }))
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { run(t, e, `{ failNullable }`, "") })
	}
	wg.Wait()
	if masked.Path != nil || masked.Locations != nil {
		t.Fatalf("the presenter's shared error was written to: path %v, locations %v", masked.Path, masked.Locations)
	}
}

// A request error cannot be dropped: data is absent, so the error list is the
// only thing saying why. A nil used to reach Response.Errors and panic.
func TestANilPresentedRequestErrorIsReplaced(t *testing.T) {
	_, e := newFixtureExecutor(t, WithErrorPresenter(func(context.Context, error) *Error { return nil }))
	resp := e.Execute(context.Background(), &Request{Query: `{ nope }`})
	if len(resp.Errors) != 1 || resp.Errors[0] == nil {
		t.Fatalf("errors = %v, want one generic error", resp.Errors)
	}
	if resp.Errors[0].Extensions["code"] != CodeValidationFailed {
		t.Errorf("code = %v, want the original %s", resp.Errors[0].Extensions["code"], CodeValidationFailed)
	}
	if _, err := json.Marshal(resp); err != nil {
		t.Fatal(err)
	}
}
