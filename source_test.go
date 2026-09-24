package graphql

import (
	"errors"
	"strings"
	"testing"
	"testing/fstest"
)

// "no SDL sources provided" is the first error a new user can hit, and nothing
// exercised it. The realistic way to get there is not calling NewSchema with
// no SDL at all -- that is obvious -- but an SDLFS pattern that matches
// nothing: the wrong directory, the wrong extension, a schema folder that was
// not embedded. The message has to say that rather than leave gqlparser to
// complain about an empty document.

func TestNewSchemaWithNoSDLSaysSo(t *testing.T) {
	for _, c := range []struct {
		name string
		src  Source
	}{
		{"no sources at all", Source{}},
		{"a glob matching nothing", SDLFS(fstest.MapFS{
			"schema/user.graphql": {Data: []byte(`type Query { ping: String! }`)},
		}, "*.graphql")},
		{"a glob in the wrong directory", SDLFS(fstest.MapFS{
			"schema/user.graphql": {Data: []byte(`type Query { ping: String! }`)},
		}, "sdl/*.graphql")},
		{"the wrong extension", SDLFS(fstest.MapFS{
			"schema/user.graphql": {Data: []byte(`type Query { ping: String! }`)},
		}, "schema/*.gql")},
		{"empty Sources", Sources()},
		{"Sources of empty sources", Sources(Source{}, Source{})},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewSchema(c.src, Query(Field("ping", func(Root) string { return "" })))
			if err == nil {
				t.Fatal("NewSchema accepted a schema with no SDL")
			}
			if !strings.Contains(err.Error(), "no SDL sources") {
				t.Errorf("error does not say the SDL is missing, so the author is left "+
					"debugging the wrong thing: %v", err)
			}
		})
	}
}

// And the control: a pattern that does match is loaded, including through
// Sources over two file systems, which is what that function exists for.
func TestSDLFSLoadsMatchingFilesAcrossFileSystems(t *testing.T) {
	a := fstest.MapFS{"a/query.graphql": {Data: []byte(`type Query { ping: String! }`)}}
	b := fstest.MapFS{"b/user.graphql": {Data: []byte(`extend type Query { name: String! }`)}}

	s, err := NewSchema(
		Sources(SDLFS(a, "a/*.graphql"), SDLFS(b, "b/*.graphql")),
		Query(
			Field("ping", func(Root) string { return "pong" }),
			Field("name", func(Root) string { return "ada" }),
		),
	)
	if err != nil {
		t.Fatalf("NewSchema: %v", err)
	}
	expectData(t, run(t, NewExecutor(s), `{ ping name }`, ""), `{"ping":"pong","name":"ada"}`)
}

// A read error names the file, rather than being swallowed into "no SDL
// sources": the two mean different things to whoever is debugging, and a
// glob that matched then failed to read is a permissions or embed problem,
// not a missing schema.
func TestAnUnreadableSDLFileIsReportedByName(t *testing.T) {
	fsys := failingFS{
		MapFS: fstest.MapFS{
			"schema/good.graphql": {Data: []byte(`type Query { ping: String! }`)},
			"schema/bad.graphql":  {Data: []byte(`type User { id: ID! }`)},
		},
		fail: "schema/bad.graphql",
		err:  errors.New("permission denied"),
	}
	_, err := NewSchema(SDLFS(fsys, "schema/*.graphql"),
		Query(Field("ping", func(Root) string { return "" })))
	if err == nil {
		t.Fatal("an unreadable SDL file was ignored")
	}
	if !strings.Contains(err.Error(), "schema/bad.graphql") {
		t.Errorf("error does not name the file that could not be read: %v", err)
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error does not carry the underlying cause: %v", err)
	}
	if strings.Contains(err.Error(), "no SDL sources") {
		t.Errorf("a read failure was reported as a missing schema: %v", err)
	}
}

// failingFS is a MapFS whose ReadFile fails for one named file, so the read
// error path is driven rather than reasoned about.
type failingFS struct {
	fstest.MapFS
	fail string
	err  error
}

// ReadFile, not Open: fs.ReadFile prefers an fs.ReadFileFS implementation, and
// the embedded MapFS provides one, so overriding Open alone is bypassed.
func (f failingFS) ReadFile(name string) ([]byte, error) {
	if name == f.fail {
		return nil, f.err
	}
	return f.MapFS.ReadFile(name)
}
