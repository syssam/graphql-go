package veloxfx

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"

	"github.com/syssam/graphql-go/examples/veloxfx/graph"
	"github.com/syssam/graphql-go/examples/veloxfx/internal/todo"
)

// The generated bindings must match the SDL velox wrote, and a clean build
// says nothing about that; ValidateSchema does, with no database behind it.
func TestSchemaBinds(t *testing.T) {
	if err := graph.ValidateSchema(scalars); err != nil {
		t.Fatal(err)
	}
}

// startApp runs the whole Module on a free port against a database no other
// test shares, and stops it when the test ends.
func startApp(t *testing.T) string {
	t.Helper()
	var srv *Server
	app := fxtest.New(t,
		Module,
		fx.Supply(Config{
			Addr: "127.0.0.1:0",
			DSN:  "file:" + t.Name() + "?mode=memory&cache=shared&_pragma=foreign_keys(1)",
		}),
		fx.Populate(&srv),
		fx.NopLogger,
	)
	app.RequireStart()
	t.Cleanup(app.RequireStop)
	return "http://" + srv.Addr() + "/graphql"
}

type response struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func post(t *testing.T, url, query string, vars map[string]any) response {
	t.Helper()
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	var out response
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%d %s: %v", res.StatusCode, raw, err)
	}
	return out
}

func mustData(t *testing.T, r response) string {
	t.Helper()
	if len(r.Errors) > 0 {
		t.Fatalf("errors: %+v", r.Errors)
	}
	return string(r.Data)
}

func TestServesThroughEveryLayer(t *testing.T) {
	url := startApp(t)

	user := mustData(t, post(t, url,
		`mutation($in: CreateUserInput!) { createUser(input: $in) { id name } }`,
		map[string]any{"in": map[string]any{"name": "Ada", "email": "ada@example.com"}}))
	if user != `{"createUser":{"id":"1","name":"Ada"}}` {
		t.Fatalf("createUser = %s", user)
	}

	// status is omitted, so the schema default applies; ownerID is an ID on
	// the wire and an int in velox's input struct.
	todo := mustData(t, post(t, url,
		`mutation($in: CreateTodoInput!) { createTodo(input: $in) { id title status owner { name } } }`,
		map[string]any{"in": map[string]any{"title": "write the example", "ownerID": "1"}}))
	if todo != `{"createTodo":{"id":"1","title":"write the example","status":"TODO","owner":{"name":"Ada"}}}` {
		t.Fatalf("createTodo = %s", todo)
	}

	done := mustData(t, post(t, url,
		`mutation { updateTodo(id: "1", input: {status: DONE}) { title status } }`, nil))
	if done != `{"updateTodo":{"title":"write the example","status":"DONE"}}` {
		t.Fatalf("updateTodo = %s", done)
	}

	all := mustData(t, post(t, url, `{ users { name todos { title status } } todos { owner { email } } }`, nil))
	if all != `{"users":[{"name":"Ada","todos":[{"title":"write the example","status":"DONE"}]}],"todos":[{"owner":{"email":"ada@example.com"}}]}` {
		t.Fatalf("query = %s", all)
	}
}

// A constraint velox enforces reaches the client as a GraphQL error on the
// field, not as a failed request.
func TestORMValidationIsAFieldError(t *testing.T) {
	url := startApp(t)
	r := post(t, url, `mutation { createUser(input: {name: "", email: "x@example.com"}) { id } }`, nil)
	if len(r.Errors) != 1 || !strings.Contains(r.Errors[0].Message, "name") {
		t.Fatalf("want one error naming the field, got %+v (data %s)", r.Errors, r.Data)
	}
}

// Stopping the app must release the port; a stop hook that returned before
// shutting the server down would leave it answering.
func TestStopClosesTheListener(t *testing.T) {
	var srv *Server
	app := fxtest.New(t,
		Module,
		fx.Supply(Config{Addr: "127.0.0.1:0", DSN: "file:" + t.Name() + "?mode=memory&cache=shared&_pragma=foreign_keys(1)"}),
		fx.Populate(&srv),
		fx.NopLogger,
	)
	app.RequireStart()
	url := "http://" + srv.Addr() + "/graphql"
	mustData(t, post(t, url, `{ users { id } }`, nil))
	app.RequireStop()

	if _, err := http.Post(url, "application/json", strings.NewReader(`{"query":"{ users { id } }"}`)); err == nil {
		t.Fatal("server still answering after stop")
	}
}

// An entity module left out of the app is a failed start that names what it
// left unbound, not a schema that builds and fails the first request to reach
// the entity. That is the whole reason the groups register themselves instead
// of filling a struct.
func TestAMissingEntityModuleFailsStart(t *testing.T) {
	a := fx.New(
		todo.Module, // user.Module left out
		app,
		fx.Supply(Config{Addr: "127.0.0.1:0", DSN: "file:" + t.Name() + "?mode=memory&cache=shared&_pragma=foreign_keys(1)"}),
		fx.NopLogger,
	)
	err := a.Err()
	if err == nil || !strings.Contains(err.Error(), "type User has no Object binding") {
		t.Fatalf("err = %v", err)
	}
}
