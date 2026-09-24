package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// repeatable collects a flag given more than once, and also splits a single
// comma-separated value, so -schema a.graphql -schema b.graphql and
// -schema a.graphql,b.graphql both work.
type repeatable []string

func (r *repeatable) String() string { return strings.Join(*r, ",") }

func (r *repeatable) Set(v string) error {
	for part := range strings.SplitSeq(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			*r = append(*r, part)
		}
	}
	return nil
}

// parseModels reads NAME=go/import/path.Type pairs. The separator is the last
// dot rather than the first, because an import path contains dots of its own:
// Time=gopkg.in/x.Time has three before the one that matters.
func parseModels(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(pairs))
	for _, p := range pairs {
		name, typ, ok := strings.Cut(p, "=")
		if !ok || name == "" || typ == "" {
			return nil, fmt.Errorf("-model %q is not NAME=go/import/path.Type", p)
		}
		out[name] = typ
	}
	return out, nil
}

// derivePackage returns the import path of dir by finding the nearest enclosing
// go.mod and joining its module path with dir's position under it. It exists so
// that the flag form needs two flags rather than three: the package of the
// output directory is not a decision, it is a fact about where the directory is.
func derivePackage(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	root := abs
	for {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			return "", fmt.Errorf("no go.mod above %s; pass -pkg with the import path of the output directory", dir)
		}
		root = parent
	}
	mod, err := modulePath(filepath.Join(root, "go.mod"))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", err
	}
	if rel == "." {
		return mod, nil
	}
	return mod + "/" + filepath.ToSlash(rel), nil
}

// modulePath reads the module line without depending on golang.org/x/mod: the
// root package's restraint about dependencies is worth keeping in its own
// tooling, and one line of go.mod does not need a parser.
func modulePath(goMod string) (string, error) {
	f, err := os.Open(goMod)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if rest, ok := strings.CutPrefix(line, "module"); ok {
			if path := strings.TrimSpace(rest); path != "" && path != "(" {
				return strings.Trim(path, `"`), nil
			}
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("%s has no module line", goMod)
}
