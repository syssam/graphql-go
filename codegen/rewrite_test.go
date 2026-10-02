package codegen

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// A file whose content did not change is not written again. Go's build cache
// is keyed by content and would not care; a file watcher, an editor's
// reload, a container layer and rsync all go by the timestamp, and a
// generator that rewrites nine hundred files to change two makes each of
// them do nine hundred files' work. Measured on 300 groups: a regenerate
// with nothing changed writes nothing, and a field added to one type writes
// that group's two files.
func TestGenerateWritesOnlyWhatChanged(t *testing.T) {
	scaffold := map[string]string{"user": "graph/user.Handler", "post": "graph/post.Handler"}
	dir, generate := wireFieldModule(t, scaffold, wireFieldDeps)
	if err := generate(); err != nil {
		t.Fatal(err)
	}
	// Every file gqlc wrote, generated code and SDL copies alike, is given
	// a timestamp in the past, so a write of any kind shows.
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	var files []string
	if err := filepath.WalkDir(filepath.Join(dir, "graph"), func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, path)
			return os.Chtimes(path, past, past)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	written := func() []string {
		t.Helper()
		var out []string
		for _, f := range files {
			info, err := os.Stat(f)
			if err != nil {
				t.Fatal(err)
			}
			if !info.ModTime().Equal(past) {
				rel, _ := filepath.Rel(dir, f)
				out = append(out, filepath.ToSlash(rel))
				if err := os.Chtimes(f, past, past); err != nil {
					t.Fatal(err)
				}
			}
		}
		slices.Sort(out)
		return out
	}
	if len(files) < 8 {
		t.Fatalf("only %d generated files; the fixture is not exercising much", len(files))
	}

	if err := generate(); err != nil {
		t.Fatal(err)
	}
	if got := written(); len(got) != 0 {
		t.Errorf("nothing changed, and these were written again: %v", got)
	}

	// One type gains a field: its group's code, its models and its SDL copy,
	// and nothing of the other group's, nor the wiring.
	sdl := filepath.Join(dir, "schema", "user.graphql")
	src := readFile(t, sdl)
	if err := os.WriteFile(sdl, []byte(strings.Replace(src, "type User {", "type User {\n  nickname: String", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := generate(); err != nil {
		t.Fatal(err)
	}
	got := written()
	if len(got) == 0 {
		t.Fatal("the schema changed and nothing was written")
	}
	for _, f := range got {
		if !strings.Contains(f, "user") {
			t.Errorf("%s was written for a change to the user group alone; all of %v", f, got)
		}
	}
}
