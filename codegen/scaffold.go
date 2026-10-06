package codegen

import (
	"fmt"
	goast "go/ast"
	"maps"
	"path/filepath"
	"slices"
	"strings"
)

// Scaffolding writes the Resolver methods an implementation does not have
// yet, as stubs with the signature the interface asks for, so adding a field
// to the SDL is generate, then fill in a body -- not read the interface and
// copy its signature by hand.
//
// It reads the implementation's package with go/parser only: method names on
// one type, nothing type-checked and nothing loaded, which keeps the promise
// that gqlc loads no Go packages unless AutoBind asks. It never edits or
// removes a method that exists. A method whose signature no longer matches is
// the compiler's to report, through the var _ Resolver assertion; one whose
// field is gone still compiles, so reportStale names it.

// scaffoldEvery is the Scaffold key that stands for every group no other key
// names, and scaffoldGroupName is where its value has the group's name.
const (
	scaffoldEvery     = "*"
	scaffoldGroupName = "{group}"
)

// expandScaffold replaces the "*" entry with one for each group that has a
// Resolver and no entry of its own, so everything after it reads a plain map.
// It runs before anything is emitted, because Wire reads the same map.
//
// Without {group} the pattern names one type for every group. That is one
// implementation asked to be all of them, which a single group allows and
// several do not: each group's stubs go to <group>.resolvers.go beside the
// type, and Wire calls one constructor per group.
func (b *builder) expandScaffold(groups []string) error {
	pattern, ok := b.cfg.Scaffold[scaffoldEvery]
	if !ok {
		return nil
	}
	keys := b.scaffoldKeys(groups)
	if len(groups) > 1 && !strings.Contains(pattern, scaffoldGroupName) {
		return fmt.Errorf("codegen: scaffold: %q is %q, which names one type for every group; write %s where the group's name goes, as in \"graph/%s.Handler\"",
			scaffoldEvery, pattern, scaffoldGroupName, scaffoldGroupName)
	}
	// A new map: the one in Config is the caller's.
	out := make(map[string]string, len(b.cfg.Scaffold)+len(keys))
	for name, target := range b.cfg.Scaffold {
		if name != scaffoldEvery {
			out[name] = target
		}
	}
	for name, group := range keys {
		if _, listed := out[name]; listed || !b.hasResolver(group) {
			continue
		}
		out[name] = strings.ReplaceAll(pattern, scaffoldGroupName, name)
	}
	b.cfg.Scaffold = out
	return nil
}

// scaffoldKeys is the group each Scaffold key stands for. A key is its
// group's name, except where there is one group or none: the output is then a
// single package, the key is that package's name, and the group is "", which
// is what the only group is called wherever a group is asked about.
func (b *builder) scaffoldKeys(groups []string) map[string]string {
	if len(groups) <= 1 {
		return map[string]string{b.pkgName: ""}
	}
	keys := make(map[string]string, len(groups))
	for _, g := range groups {
		keys[g] = g
	}
	return keys
}

// checkScaffold refuses a key that is no group with a Resolver -- a typo
// there would otherwise scaffold nothing and say nothing -- and a target
// that is not dir.Type. Generate calls it before writing anything.
func (b *builder) checkScaffold(groups []string) error {
	if len(b.cfg.Scaffold) == 0 {
		return nil
	}
	keys := b.scaffoldKeys(groups)
	for _, name := range slices.Sorted(maps.Keys(b.cfg.Scaffold)) {
		if group, known := keys[name]; !known || !b.hasResolver(group) {
			return fmt.Errorf("codegen: scaffold: %q is not a group with a Resolver (groups: %s)",
				name, strings.Join(slices.Sorted(maps.Keys(keys)), ", "))
		}
		if _, _, err := b.splitTarget(b.cfg.Scaffold[name]); err != nil {
			return fmt.Errorf("codegen: scaffold %s: %w", name, err)
		}
	}
	return nil
}

// scaffold writes stubs for every group Config.Scaffold names. Its refusals
// have been made by then, before anything was written; checkScaffold is
// asked again because scaffold is also reached on its own.
func (b *builder) scaffold(groups []string) error {
	if len(b.cfg.Scaffold) == 0 {
		return nil
	}
	if err := b.checkScaffold(groups); err != nil {
		return err
	}
	// The output is on disk by now and stubs are about to be: see packageName.
	b.scaffolding = true
	keys := b.scaffoldKeys(groups)
	names := slices.Sorted(maps.Keys(b.cfg.Scaffold))
	pkgs := &pkgCache{byFold: map[string][]*pkgIndex{}}
	methods := make(map[string][]string, len(names))
	for _, name := range names {
		ms, err := b.scaffoldGroup(pkgs, name, keys[name], b.cfg.Scaffold[name])
		if err != nil {
			return fmt.Errorf("codegen: scaffold %s: %w", name, err)
		}
		methods[name] = ms
	}

	// One type can implement several groups, so a method is stale only when
	// none of them asks for it, and it is reported once per implementation: a
	// type serving three hundred groups is one note, not three hundred. A
	// target is one type in one directory however its path is spelled --
	// "impl", "./impl", and "Impl" where the filesystem ignores case, but not
	// where it does not -- so this runs after scaffolding, when every target
	// directory exists and the filesystem can say which spellings are one.
	// pkgCache already resolved every spelling of a directory to one index, so
	// an implementation is one index and one type.
	type key struct {
		idx *pkgIndex
		typ string
	}
	type impl struct {
		spelled string
		groups  []string
		want    map[string]bool
	}
	var order []key
	impls := map[key]*impl{}
	for _, name := range names {
		dir, typ, _ := b.splitTarget(b.cfg.Scaffold[name]) // validated by scaffoldGroup
		idx, err := pkgs.get(b, dir)
		if err != nil {
			return fmt.Errorf("codegen: scaffold %s: %w", name, err)
		}
		k := key{idx, typ}
		t := impls[k]
		if t == nil {
			t = &impl{spelled: b.cfg.Scaffold[name], want: map[string]bool{}}
			impls[k] = t
			order = append(order, k)
		}
		t.groups = append(t.groups, name)
		for _, m := range methods[name] {
			t.want[m] = true
		}
	}
	for _, k := range order {
		t := impls[k]
		b.reportStale(t.groups, t.spelled, k.idx.methods[k.typ], t.want)
	}
	return nil
}

// splitTarget reads a Scaffold target, "dir.Type" with dir relative to
// Config.Dir, into the cleaned absolute directory and the type.
func (b *builder) splitTarget(target string) (dir, typ string, err error) {
	i := strings.LastIndex(target, ".")
	if i <= 0 || i == len(target)-1 {
		return "", "", fmt.Errorf("target %q is not dir.Type", target)
	}
	return filepath.Join(b.dir, filepath.FromSlash(target[:i])), target[i+1:], nil
}

// reportStale names the exported methods on the implementation that its
// Resolver does not have. A field removed from the SDL leaves its method
// behind, compiling, because the var _ Resolver assertion checks only what
// the interface asks for; this run is the one that knows. It is a note, not
// an error: an exported method can be the author's own, such as String.
// want holds the methods of every Resolver the type implements -- one per
// group in groups -- since a type serving two groups has both groups' methods
// and neither group's are stale.
func (b *builder) reportStale(groups []string, target string, have, want map[string]bool) {
	var stale []string
	for m := range have {
		if !want[m] && goast.IsExported(m) {
			stale = append(stale, m)
		}
	}
	if len(stale) == 0 {
		return
	}
	slices.Sort(stale)
	which := "the " + groups[0] + " Resolver does not"
	if len(groups) > 1 {
		which = "none of the " + strings.Join(groups, ", ") + " Resolvers has"
	}
	b.notef("scaffold %s: %s has methods %s: %s. If the SDL dropped their fields, delete them.",
		strings.Join(groups, ", "), target, which, strings.Join(stale, ", "))
}
