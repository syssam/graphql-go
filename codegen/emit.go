package codegen

import (
	"errors"
	"fmt"
	"strings"
)

func (b *builder) emit() (map[string][]byte, error) {
	b.reportUnboundScalars()
	files := map[string][]byte{}
	// The copies are embedded by glob from one directory, so each is keyed by
	// its name there, and two sources sharing one would leave a single copy
	// for both: the output compiled and the schema failed at start-up on the
	// types of whichever file was dropped. A name is compared without letter
	// case, because the filesystem the output lands on may compare it so:
	// there User.graphql and user.graphql were one file, written twice at
	// once, holding parts of both.
	first := map[string]string{}
	for _, src := range b.sources {
		name := copyName(src.Name)
		if prev, dup := first[strings.ToLower(name)]; dup {
			return nil, fmt.Errorf("codegen: schema sources %s and %s would both be copied to schema/%s -- a copy is named after its source, without its extension, and without regard to letter case -- and the generated package embeds one copy per name; rename one of them", prev, src.Name, name)
		}
		first[strings.ToLower(name)] = src.Name
		files["schema/"+name] = []byte(src.Input)
	}
	groups := b.uniqueGroups()
	// Before anything is written: a name collision here would otherwise arrive
	// as a compile error in a file the author is told not to edit.
	if err := errors.Join(b.checkNameCollisions(groups), b.checkIdentifiers()); err != nil {
		return nil, err
	}
	// One model package per group keeps a one-group schema edit from
	// invalidating every other group's package. Input objects are the only
	// model types that can reference across groups, so a cycle among them
	// forces the single shared package instead.
	b.modelSplit = len(groups) > 1 && b.modelGroupsAcyclic(groups)
	// After the split is known, because a constant collides only with a type
	// in the same model package.
	if err := b.checkEnumConstants(); err != nil {
		return nil, err
	}
	if b.modelSplit {
		for _, g := range groups {
			src := b.emitModel(g)
			if src == "" {
				continue
			}
			if err := putGo(files, "model/"+g+"/models.go", src); err != nil {
				return nil, err
			}
		}
	} else if err := putGo(files, "model/models.go", b.emitModel("")); err != nil {
		return nil, err
	}
	if !b.cfg.Wire.IsZero() {
		src, err := b.emitWire(groups)
		if err != nil {
			return nil, err
		}
		if err := putGo(files, "wire.go", src); err != nil {
			return nil, err
		}
	}
	if len(groups) <= 1 {
		if err := putGo(files, "generated.go", b.emitGroup(b.pkgName, "", true)); err != nil {
			return nil, err
		}
		if err := putGo(files, "schema.go", b.emitSchema()); err != nil {
			return nil, err
		}
		return files, nil
	}
	for _, g := range groups {
		if err := putGo(files, b.groupRel(g)+"/generated.go", b.emitGroup(g, g, b.hasResolver(g))); err != nil {
			return nil, err
		}
	}
	if err := putGo(files, "schema.go", b.emitSchemaGrouped(groups)); err != nil {
		return nil, err
	}
	return files, nil
}
