package howto

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pageSources names the Go files each task page is written from. A page not
// listed here fails the test, so a fifth page cannot be added without code
// behind it.
var pageSources = map[string][]string{
	"n-plus-one.md":       {"loader.go", "loader_test.go"},
	"authorization.md":    {"authz.go", "authz_test.go"},
	"custom-scalar.md":    {"scalar.go", "scalar_test.go"},
	"testing-a-server.md": {"server.go", "server_test.go"},
}

// Every Go block on a task page has to be in the page's Go files, which
// compile and whose tests pass. A page that showed code of its own would be
// right on the day it was written and unchecked ever after.
func TestPagesShowOnlyCodeThatExists(t *testing.T) {
	dir := filepath.Join("..", "..", "docs", "tasks")
	pages, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	// Counted, because a wrong directory globs to nothing and a loop over
	// nothing passes.
	if len(pages) != len(pageSources) {
		t.Fatalf("found %d pages in %s, and pageSources names %d", len(pages), dir, len(pageSources))
	}

	for _, page := range pages {
		name := filepath.Base(page)
		t.Run(name, func(t *testing.T) {
			sources, ok := pageSources[name]
			if !ok {
				t.Fatalf("pageSources names no Go files for %s", name)
			}
			var code []string
			for _, src := range sources {
				b, err := os.ReadFile(src)
				if err != nil {
					t.Fatal(err)
				}
				// A blank line between files, so that no block can match
				// across the end of one and the start of the next.
				code = append(code, strings.Split(string(b), "\n")...)
				code = append(code, "")
			}

			md, err := os.ReadFile(page)
			if err != nil {
				t.Fatal(err)
			}
			blocks, err := goBlocks(string(md))
			if err != nil {
				t.Fatal(err)
			}
			if len(blocks) == 0 {
				t.Fatal("no Go block found; either the page has no code or the extraction is not reading it")
			}
			for _, b := range blocks {
				if !containsBlock(code, b.lines) {
					t.Errorf("%s:%d: this block is not in %s:\n%s",
						name, b.line, strings.Join(sources, " or "), strings.Join(b.lines, "\n"))
				}
			}
		})
	}
}

type block struct {
	line  int // of the opening fence, counted from 1
	lines []string
}

// goBlocks returns the body of every ```go fence in a Markdown document.
func goBlocks(md string) ([]block, error) {
	var (
		out  []block
		cur  *block
		skip bool // inside a fence of another language
	)
	for i, line := range strings.Split(md, "\n") {
		fence := strings.TrimSpace(line)
		switch {
		case cur != nil && fence == "```":
			if len(trimBlank(cur.lines)) == 0 {
				return nil, fmt.Errorf("line %d: empty Go block", cur.line)
			}
			out = append(out, *cur)
			cur = nil
		case cur != nil:
			cur.lines = append(cur.lines, line)
		case skip:
			skip = fence != "```"
		case fence == "```go":
			cur = &block{line: i + 1}
		case strings.HasPrefix(fence, "```"):
			skip = true
		}
	}
	if cur != nil {
		return nil, fmt.Errorf("line %d: Go block is never closed", cur.line)
	}
	return out, nil
}

// containsBlock reports whether want appears in code as consecutive lines.
// Blank lines at either end of want are ignored, and so is indentation the
// whole of want shares, because a page may show the middle of a function
// without the tabs it sits behind. Indentation within want is not ignored:
// every line has to sit as far in from the first as it does in the file.
func containsBlock(code, want []string) bool {
	want = dedent(trimBlank(want))
	if len(want) == 0 {
		return false
	}
	for start := range code {
		if start+len(want) > len(code) {
			return false
		}
		// What the file has in front of the block's first line is the
		// indentation every other line must then carry.
		indent, found := strings.CutSuffix(trimRight(code[start]), want[0])
		if !found || strings.TrimSpace(indent) != "" {
			continue
		}
		matched := true
		for i, w := range want {
			got := trimRight(code[start+i])
			if w != "" {
				w = indent + w
			}
			if got != w {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func trimRight(s string) string { return strings.TrimRight(s, " \t\r") }

func trimBlank(lines []string) []string {
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// dedent removes the leading whitespace every non-blank line shares.
func dedent(lines []string) []string {
	shared, first := "", true
	for _, l := range lines {
		l = trimRight(l)
		if l == "" {
			continue
		}
		lead := l[:len(l)-len(strings.TrimLeft(l, " \t"))]
		if first {
			shared, first = lead, false
			continue
		}
		for !strings.HasPrefix(lead, shared) {
			shared = shared[:len(shared)-1]
		}
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = strings.TrimPrefix(trimRight(l), shared)
	}
	return out
}

// The check above is only worth having if it refuses a block that is nearly
// right, so the matcher is tested on its own: against a page that is correct
// every case here would go unexercised.
func TestContainsBlock(t *testing.T) {
	code := strings.Split("package p\n\nfunc f() {\n\tif ok {\n\t\treturn 1\n\t}\n\n\treturn 2\n}\n", "\n")

	for name, c := range map[string]struct {
		block string
		want  bool
	}{
		"a whole function":                {"func f() {\n\tif ok {\n\t\treturn 1\n\t}\n\n\treturn 2\n}", true},
		"an excerpt, as indented":         {"\tif ok {\n\t\treturn 1\n\t}", true},
		"an excerpt, indentation removed": {"if ok {\n\treturn 1\n}", true},
		"blank lines around it":           {"\n\nif ok {\n\treturn 1\n}\n\n", true},
		"across a blank line":             {"}\n\nreturn 2", true},
		"one character changed":           {"if ok {\n\treturn 3\n}", false},
		"a line left out":                 {"if ok {\n}", false},
		"a blank line left out":           {"}\nreturn 2", false},
		"inner indentation flattened":     {"if ok {\nreturn 1\n}", false},
		"only the end of a line":          {"ok {\n\treturn 1\n}", false},
		"only the start of a line":        {"if ok {\n\treturn", false},
		"nothing":                         {"\n\n", false},
	} {
		if got := containsBlock(code, strings.Split(c.block, "\n")); got != c.want {
			t.Errorf("%s: containsBlock = %v, want %v", name, got, c.want)
		}
	}
}

func TestGoBlocks(t *testing.T) {
	md := "text\n```go\na := 1\n```\n```sh\ngo test\n```\n\n```go\nb := 2\n\nc := 3\n```\n"
	blocks, err := goBlocks(md)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocks) != 2 || blocks[0].line != 2 || strings.Join(blocks[1].lines, "|") != "b := 2||c := 3" {
		t.Fatalf("blocks = %+v", blocks)
	}
	if _, err := goBlocks("```go\na := 1\n"); err == nil {
		t.Fatal("an unclosed block was accepted")
	}
	if _, err := goBlocks("```go\n\n```\n"); err == nil {
		t.Fatal("an empty block was accepted")
	}
}
