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
	stubs := map[string]bool{}
	for _, k := range order {
		t := impls[k]
		b.reportStale(t.groups, t.spelled, k.idx.methods[k.typ], t.want)
		maps.Copy(stubs, k.idx.stubs[k.typ])
	}
	b.reportStubs(stubs)
	return nil
}

// reportStubs names the Resolver methods still holding the panic scaffolding
// wrote, as one note for the whole run however many groups hold them: a
// project with hundreds of groups and stubs left on purpose would otherwise
// read a line per group on every generate. CheckOptions.Stubs lists them all.
func (b *builder) reportStubs(stubs map[string]bool) {
	if len(stubs) == 0 {
		return
	}
	const shown = 10
	coords := slices.Sorted(maps.Keys(stubs))
	more := ""
	if len(coords) > shown {
		more = fmt.Sprintf(" and %d more", len(coords)-shown)
		coords = coords[:shown]
	}
	b.notef("scaffold: %d Resolver method(s) still hold the panic gqlc wrote, which a query reaching them returns as an error: %s%s. gqlc -check -stubs fails while any do.",
		len(stubs), strings.Join(coords, ", "), more)
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
	stale := staleMethods(have, want)
	if len(stale) == 0 {
		return
	}
	which := "the " + groups[0] + " Resolver does not"
	if len(groups) > 1 {
		which = "none of the " + strings.Join(groups, ", ") + " Resolvers has"
	}
	b.notef("scaffold %s: %s has methods %s: %s. If the SDL dropped their fields, delete them.",
		strings.Join(groups, ", "), target, which, strings.Join(stale, ", "))
}

// staleMethods is the exported methods in have that want does not, sorted:
// what reportStale names and CheckOptions.Orphans fails on, so the two cannot
// disagree about what an orphan is.
func staleMethods(have, want map[string]bool) []string {
	var stale []string
	for m := range have {
		if !want[m] && goast.IsExported(m) {
			stale = append(stale, m)
		}
	}
	slices.Sort(stale)
	return stale
}

// checkOrphans is CheckOptions.Orphans: every exported method of a Scaffold
// target that none of the Resolvers the type implements has, read from the
// files checkScaffoldTargets parsed. Like checkStubs it matches directories as
// the filesystem resolves them, so two spellings of one target are one type,
// and a type serving several groups is judged by all of their Resolvers.
func (b *builder) checkOrphans() error {
	byDir := map[string][]string{}
	dirs := b.newDirSet()
	for _, path := range slices.Sorted(maps.Keys(b.checked)) {
		d := filepath.Dir(path)
		if byDir[d] == nil {
			dirs.add(d)
		}
		byDir[d] = append(byDir[d], path)
	}
	keys := b.scaffoldKeys(b.uniqueGroups())
	type impl struct {
		dir, typ, target string
		want             map[string]bool
	}
	var order []string
	impls := map[string]*impl{}
	for _, name := range slices.Sorted(maps.Keys(b.cfg.Scaffold)) {
		target := b.cfg.Scaffold[name]
		dir, typ, err := b.splitTarget(target)
		if err != nil {
			continue
		}
		d, ok := dirs.find(dir)
		if !ok {
			continue
		}
		k := d + "\x00" + typ
		t := impls[k]
		if t == nil {
			t = &impl{dir: d, typ: typ, target: target, want: map[string]bool{}}
			impls[k] = t
			order = append(order, k)
		}
		for _, m := range b.resolverSignatures(keys[name], "") {
			t.want[m.name] = true
		}
	}
	var lines []string
	for _, k := range order {
		t := impls[k]
		have := map[string]bool{}
		file := map[string]string{}
		for _, path := range byDir[t.dir] {
			for _, m := range b.checked[path].methods {
				if m[0] == t.typ {
					have[m[1]] = true
					file[m[1]] = path
				}
			}
		}
		for _, m := range staleMethods(have, t.want) {
			rel, err := filepath.Rel(b.dir, file[m])
			if err != nil {
				rel = file[m]
			}
			lines = append(lines, fmt.Sprintf("  %s (%s.%s, %s)", m, filepath.ToSlash(t.target[:strings.LastIndex(t.target, ".")]), t.typ, filepath.ToSlash(rel)))
		}
	}
	if len(lines) == 0 {
		return nil
	}
	return fmt.Errorf("%w; %d method(s), each a field dropped from the SDL or an exported method of the author's own:\n%s", ErrOrphans, len(lines), strings.Join(lines, "\n"))
}

// checkStubs is CheckOptions.Stubs: every method of a Scaffold target still
// holding its scaffolded body, read from the files checkScaffoldTargets
// parsed, which are every file of each target's package that gqlc did not
// write.
func (b *builder) checkStubs() error {
	// A directory is matched as the filesystem resolves it (dirSet), since
	// checkScaffoldTargets parsed it under the first spelling a target gave;
	// two spellings of one directory then find the same files, and a line is
	// kept once. The files are grouped by directory first: matching every
	// target against every file was two stats a pair.
	byDir := map[string][]string{}
	dirs := b.newDirSet()
	for _, path := range slices.Sorted(maps.Keys(b.checked)) {
		d := filepath.Dir(path)
		if byDir[d] == nil {
			dirs.add(d)
		}
		byDir[d] = append(byDir[d], path)
	}
	var lines []string
	seen := map[string]bool{}
	for _, name := range slices.Sorted(maps.Keys(b.cfg.Scaffold)) {
		target := b.cfg.Scaffold[name]
		dir, typ, err := b.splitTarget(target)
		if err != nil {
			continue
		}
		d, ok := dirs.find(dir)
		if !ok {
			continue
		}
		for _, path := range byDir[d] {
			for _, st := range b.checked[path].stubs {
				if st[0] != typ {
					continue
				}
				rel, err := filepath.Rel(b.dir, path)
				if err != nil {
					rel = path
				}
				line := fmt.Sprintf("  %s (%s.%s, %s)", st[1], filepath.ToSlash(target[:strings.LastIndex(target, ".")]), typ, filepath.ToSlash(rel))
				if key := path + "\x00" + st[1]; !seen[key] {
					seen[key] = true
					lines = append(lines, line)
				}
			}
		}
	}
	if len(lines) == 0 {
		return nil
	}
	return fmt.Errorf("%w; %d still panic as scaffolded:\n%s", ErrStubs, len(lines), strings.Join(lines, "\n"))
}
