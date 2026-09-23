package graphql

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// An extension value that does not marshal fails the whole envelope. Extensions
// are a public, unchecked map -- WithExtension takes `any` -- so this is one
// application mistake away, and the branch that reports it had no test.
//
// This pins the behaviour rather than endorsing it. Nothing is written, so a
// caller that checks the error can still send something of its own; but the
// HTTP transports call WriteTo after the status header is on the wire and can
// only log, which leaves the client an empty 200 with no explanation and no
// connection to the cause. Whether that should instead degrade -- drop the
// offending extension and write the rest -- is a decision, not an accident,
// and this test is what makes changing it a deliberate act.
func TestAnUnmarshalableExtensionFailsTheEnvelope(t *testing.T) {
	_, e := newFixtureExecutor(t, WithErrorPresenter(
		func(ctx context.Context, err error) *Error {
			return DefaultErrorPresenter(ctx, err).WithExtension("bad", make(chan int))
		}))
	resp := run(t, e, `{ fail }`, "")
	if len(resp.Errors) == 0 {
		t.Fatal("the fixture's failing field produced no error")
	}

	if _, err := resp.MarshalJSON(); err == nil {
		t.Error("MarshalJSON accepted an unmarshalable extension")
	} else if !strings.Contains(err.Error(), "extensions") {
		t.Errorf("error does not say which member failed: %v", err)
	}

	var buf bytes.Buffer
	n, err := resp.WriteTo(&buf)
	if err == nil {
		t.Fatal("WriteTo accepted an unmarshalable extension")
	}
	if n != 0 || buf.Len() != 0 {
		t.Errorf("WriteTo wrote %d bytes (%q) before failing; a truncated envelope is "+
			"worse than none, because a client cannot tell it is truncated",
			n, buf.String())
	}
}

// The same on the response's own extensions, which is the other public map.
func TestAnUnmarshalableResponseExtensionFailsTheEnvelope(t *testing.T) {
	_, e := newFixtureExecutor(t, WithOperationInterceptor(OperationInterceptorFunc(
		func(ctx context.Context, oc *OperationContext, next OperationHandler) *Response {
			// Before next: the response copies the operation's extensions
			// when it is built, so setting one afterwards is too late.
			oc.SetExtension("bad", func() {})
			return next(ctx, oc)
		})))
	resp := run(t, e, `{ me { id } }`, "")
	var buf bytes.Buffer
	if _, err := resp.WriteTo(&buf); err == nil {
		t.Fatal("WriteTo accepted an unmarshalable response extension")
	}
	if buf.Len() != 0 {
		t.Errorf("WriteTo wrote %q before failing", buf.String())
	}
}

// And the control: a response whose extensions do marshal still writes, so the
// tests above are pinning the failure and not a WriteTo that never works.
func TestAMarshalableExtensionStillWrites(t *testing.T) {
	_, e := newFixtureExecutor(t, WithErrorPresenter(
		func(ctx context.Context, err error) *Error {
			return DefaultErrorPresenter(ctx, err).WithExtension("ok", map[string]any{"n": 1})
		}))
	resp := run(t, e, `{ fail }`, "")
	var buf bytes.Buffer
	n, err := resp.WriteTo(&buf)
	if err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	if n == 0 || !strings.Contains(buf.String(), `"ok":{"n":1}`) {
		t.Errorf("the extension did not reach the envelope: %s", buf.String())
	}
}
