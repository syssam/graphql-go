package transport_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/gofiber/fiber/v3"

	"github.com/syssam/graphql-go/transport/gqlfiber"
	"github.com/syssam/graphql-go/transport/gqlws"
)

// originCase is one upgrade request and whether it may open a connection.
type originCase struct {
	name string
	// origin is the Origin header; "" sends none, and "SELF" stands for the
	// server's own address.
	origin string
	allow  bool
}

// originDrivers starts both WebSocket handlers with the same origin policy
// and returns their URLs. gqlws leaves the check to coder/websocket; gqlfiber
// has no such hook under it and carries a copy written to mirror it. A
// browser opens a WebSocket to any site with the user's cookies and no
// preflight, so the two disagreeing is one of them admitting a page the
// operator did not name.
func originDrivers(t *testing.T, patterns []string, skip bool) map[string]string {
	t.Helper()
	var wsOpts []gqlws.Option
	var fiberOpts []gqlfiber.Option
	if patterns != nil {
		wsOpts = append(wsOpts, gqlws.WithOriginPatterns(patterns...))
		fiberOpts = append(fiberOpts, gqlfiber.WithOriginPatterns(patterns...))
	}
	if skip {
		wsOpts = append(wsOpts, gqlws.WithInsecureSkipOriginCheck())
		fiberOpts = append(fiberOpts, gqlfiber.WithInsecureSkipOriginCheck())
	}
	srv := httptest.NewServer(gqlws.New(newEquivExecutor(t), wsOpts...))
	t.Cleanup(srv.Close)
	app := fiber.New()
	app.Get("/", gqlfiber.WS(newEquivExecutor(t), fiberOpts...))
	return map[string]string{"gqlws": srv.URL, "gqlfiber": startFiberEquiv(t, app)}
}

// upgrades reports whether base accepts a WebSocket upgrade carrying origin.
func upgrades(t *testing.T, base, origin string) bool {
	t.Helper()
	header := http.Header{}
	switch origin {
	case "":
	case "SELF":
		header.Set("Origin", base)
	default:
		header.Set("Origin", origin)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http"), &websocket.DialOptions{
		Subprotocols: []string{gqlws.Subprotocol},
		HTTPHeader:   header,
	})
	if err != nil {
		return false
	}
	_ = ws.CloseNow()
	return true
}

// SECURITY.md puts a bypass of the WebSocket origin check in scope, and until
// this table only gqlfiber's copy of the check had a test: turning gqlws's off
// by default left its suite, gqlmux's and gqlecho's green. Each row is asked
// of both drivers and has one right answer, so the copy is compared with what
// it copies rather than trusted to match it.
func TestWebSocketOriginCheck(t *testing.T) {
	for _, policy := range []struct {
		name     string
		patterns []string
		skip     bool
		cases    []originCase
	}{
		{"the default", nil, false, []originCase{
			{"no Origin is not a browser", "", true},
			{"the server's own origin", "SELF", true},
			{"another site", "http://evil.example", false},
			{"an opaque origin", "null", false},
			{"the server's host under another port", "http://127.0.0.1:1", false},
			{"something that is not a URL", "http://%zz", false},
		}},
		{"a host pattern", []string{"*.example"}, false, []originCase{
			{"a host it matches", "http://app.example", true},
			{"in another case", "http://APP.Example", true},
			{"under either scheme", "https://app.example", true},
			{"the server's own origin still", "SELF", true},
			{"a suffix beyond the pattern", "http://app.example.com", false},
			{"a host that only ends like it", "http://app.example.attacker.net", false},
			{"a port the pattern does not carry", "http://app.example:8080", false},
			{"another site", "http://evil.test", false},
		}},
		{"an exact host", []string{"app.example"}, false, []originCase{
			{"the host", "https://app.example", true},
			{"a label in front of it", "http://evil.app.example", false},
			{"a label behind it", "http://app.example.evil.test", false},
		}},
		{"a pattern that pins the scheme", []string{"https://app.example"}, false, []originCase{
			{"that scheme", "https://app.example", true},
			{"the other scheme", "http://app.example", false},
		}},
		{"the check turned off", nil, true, []originCase{
			{"another site", "http://evil.example", true},
			{"an opaque origin", "null", true},
		}},
	} {
		t.Run(policy.name, func(t *testing.T) {
			drivers := originDrivers(t, policy.patterns, policy.skip)
			for _, tc := range policy.cases {
				t.Run(tc.name, func(t *testing.T) {
					for _, name := range []string{"gqlws", "gqlfiber"} {
						if got := upgrades(t, drivers[name], tc.origin); got != tc.allow {
							t.Errorf("%s: upgrade accepted = %v, want %v (Origin %q)", name, got, tc.allow, tc.origin)
						}
					}
				})
			}
		})
	}
}
