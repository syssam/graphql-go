#!/usr/bin/env python3
"""Writes a synthetic N-entity service in one of three wiring layouts.

  fx     handlers and their bindings registered with fx
  deps   every handler takes the whole Services struct
  field  every handler takes its own service; gqlc's Wire passes d.<Entity>

Usage: gen.py <layout> <n> <dir> <repo> <gqlc-binary>
"""
import os, subprocess, sys

layout, n, out, repo, gqlc = sys.argv[1], int(sys.argv[2]), sys.argv[3], sys.argv[4], sys.argv[5]
names = ["e%03d" % i for i in range(n)]


def write(rel, body):
    p = os.path.join(out, rel)
    os.makedirs(os.path.dirname(p), exist_ok=True)
    with open(p, "w") as f:
        f.write(body)


write("go.mod", f"""module scale

go 1.27

require (
	github.com/syssam/graphql-go v0.0.0
	go.uber.org/fx v1.24.0
)

replace github.com/syssam/graphql-go => {repo}
""")

for i, e in enumerate(names):
    E = e.upper()
    pre = "" if i == 0 else "extend "
    write(f"sdl/{e}.graphql", f"""type {E} {{ id: ID! name: String! code: String! qty: Int! active: Boolean! note: String }}
input {E}Input {{ name: String! code: String! qty: Int! active: Boolean! note: String }}
{pre}type Query {{ {e}(id: ID!): {E}  {e}s(first: Int): [{E}!]! }}
{pre}type Mutation {{ create{E}(input: {E}Input!): {E}!  update{E}(id: ID!, input: {E}Input!): {E}! }}
""")

scaffold = "".join(f"  {e}: graph/{e}.Handler\n" for e in names)
# gqlc writes wire.go for the field layout. For deps it is written below by
# hand: gqlc hands a group the field named after it, so the layout that passes
# the whole struct is one it no longer generates.
wire = "wire:\n  deps: scale/internal/service.Services\n" if layout == "field" else ""
write("gqlc.yaml", f"""schema: [sdl/*.graphql]
output: graph
package: scale/graph
scaffold:
{scaffold}{wire}""")

# The store every service is built from, and the services: one package each.
write("internal/store/store.go", """// Package store stands in for a database client.
package store

import "sync"

type DB struct {
	mu   sync.Mutex
	rows map[string]map[string]any
}

func New() *DB { return &DB{rows: map[string]map[string]any{}} }

func (d *DB) Get(table, id string) any {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.rows[table][id]
}

func (d *DB) Put(table, id string, row any) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.rows[table] == nil {
		d.rows[table] = map[string]any{}
	}
	d.rows[table][id] = row
}

func (d *DB) All(table string) []any {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]any, 0, len(d.rows[table]))
	for _, r := range d.rows[table] {
		out = append(out, r)
	}
	return out
}
""")

for e in names:
    E = e.upper()
    write(f"internal/service/{e}/{e}.go", f"""// Package {e} is the {e} service.
package {e}

import (
	"context"
	"errors"
	"fmt"

	graphql "github.com/syssam/graphql-go"

	model "scale/graph/model/{e}"
	"scale/internal/store"
)

type Service struct{{ db *store.DB }}

func New(db *store.DB) *Service {{ return &Service{{db: db}} }}

func (s *Service) Get(ctx context.Context, id string) (*model.{E}, error) {{
	row, _ := s.db.Get("{e}", id).(*model.{E})
	return row, nil
}}

func (s *Service) List(ctx context.Context, first *int) ([]*model.{E}, error) {{
	rows := s.db.All("{e}")
	out := make([]*model.{E}, 0, len(rows))
	for _, r := range rows {{
		if first != nil && len(out) >= *first {{
			break
		}}
		out = append(out, r.(*model.{E}))
	}}
	return out, nil
}}

func (s *Service) Create(ctx context.Context, in model.{E}Input) (*model.{E}, error) {{
	if in.Qty < 0 {{
		return nil, errors.New("qty must not be negative")
	}}
	row := &model.{E}{{ID: graphql.ID(in.Code), Name: in.Name, Code: in.Code, Qty: in.Qty, Active: in.Active, Note: in.Note}}
	s.db.Put("{e}", in.Code, row)
	return row, nil
}}

func (s *Service) Update(ctx context.Context, id string, in model.{E}Input) (*model.{E}, error) {{
	row, _ := s.db.Get("{e}", id).(*model.{E})
	if row == nil {{
		return nil, fmt.Errorf("{e} %s not found", id)
	}}
	row.Name, row.Qty, row.Active, row.Note = in.Name, in.Qty, in.Active, in.Note
	return row, nil
}}
""")

# The handlers, written before gqlc runs so scaffolding finds them complete.
for e in names:
    E = e.upper()
    if layout == "deps":
        imports = f'\t"scale/internal/service"\n\tsvc "scale/internal/service/{e}"\n'
        ctor = f"func NewHandler(s service.Services) *Handler {{ return &Handler{{svc: s.{E}}} }}"
    else:
        imports = f'\tsvc "scale/internal/service/{e}"\n'
        ctor = "func NewHandler(s *svc.Service) *Handler { return &Handler{svc: s} }"
    write(f"graph/{e}/{e}.resolvers.go", f"""package {e}

import (
	"context"

	model "scale/graph/model/{e}"
{imports})

// Handler implements the {e} group's Resolver.
type Handler struct{{ svc *svc.Service }}

{ctor}

var _ Resolver = (*Handler)(nil)

func (r *Handler) Create{E}(ctx context.Context, args Create{E}Args) (*model.{E}, error) {{
	return r.svc.Create(ctx, args.Input)
}}

func (r *Handler) Update{E}(ctx context.Context, args Update{E}Args) (*model.{E}, error) {{
	return r.svc.Update(ctx, string(args.ID), args.Input)
}}

func (r *Handler) {E}(ctx context.Context, args {E}Args) (*model.{E}, error) {{
	return r.svc.Get(ctx, string(args.ID))
}}

func (r *Handler) {E}s(ctx context.Context, args {E}sArgs) ([]*model.{E}, error) {{
	return r.svc.List(ctx, args.First)
}}
""")

svc_imports = "".join(f'\t"scale/internal/service/{e}"\n' for e in names)
provides = "".join(f"\t\t{e}.New,\n" for e in names)
fields = "".join(f"\t{e.upper()} *{e}.Service\n" for e in names)
services = "" if layout == "fx" else f"""
// Services is every service, filled in by fx.
type Services struct {{
	fx.In

{fields}}}
"""
write("internal/service/service.go", f"""// Package service provides every service.
package service

import (
	"go.uber.org/fx"

{svc_imports})
{services}
// Module provides every service.
var Module = fx.Module("service",
	fx.Provide(
{provides}	),
)
""")

graph_imports = "".join(f'\t"scale/graph/{e}"\n' for e in names)
if layout == "fx":
    write("internal/gqlfx/gqlfx.go", """// Package gqlfx registers GraphQL groups with fx.
package gqlfx

import (
	"go.uber.org/fx"

	graphql "github.com/syssam/graphql-go"
)

func Register[R any](newResolver any, bindings func(R) graphql.SchemaOption) fx.Option {
	return fx.Provide(
		fx.Annotate(newResolver, fx.As(new(R))),
		fx.Annotate(bindings, fx.ResultTags(`group:"graphql"`)),
	)
}

type Groups struct {
	fx.In
	Bindings []graphql.SchemaOption `group:"graphql"`
}
""")
    regs = "".join(f"\tgqlfx.Register({e}.NewHandler, {e}.Bindings),\n" for e in names)
    write("app/app.go", f"""// Package app assembles the service.
package app

import (
	"go.uber.org/fx"

	graphql "github.com/syssam/graphql-go"

	"scale/graph"
{graph_imports}	"scale/internal/gqlfx"
	"scale/internal/service"
	"scale/internal/store"
)

var Resolvers = fx.Module("resolver",
{regs})

func NewSchema(g gqlfx.Groups) (*graphql.Schema, error) {{ return graph.NewSchema(g.Bindings...) }}

var Module = fx.Module("app", service.Module, Resolvers, fx.Provide(store.New, NewSchema))
""")
else:
    write("app/app.go", """// Package app assembles the service.
package app

import (
	"go.uber.org/fx"

	graphql "github.com/syssam/graphql-go"

	"scale/graph"
	"scale/internal/service"
	"scale/internal/store"
)

func NewSchema(s service.Services) (*graphql.Schema, error) { return graph.NewSchema(graph.Wire(s)...) }

var Module = fx.Module("app", service.Module, fx.Provide(store.New, NewSchema))
""")

write("cmd/server/main.go", """// Command server builds the app, reports what that cost, and answers one query.
package main

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"time"

	"go.uber.org/fx"

	graphql "github.com/syssam/graphql-go"

	"scale/app"
)

func heap() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

func main() {
	before := heap()
	var total runtime.MemStats
	runtime.ReadMemStats(&total)
	allocBefore, mallocsBefore := total.TotalAlloc, total.Mallocs
	start := time.Now()
	var s *graphql.Schema
	a := fx.New(app.Module, fx.Populate(&s), fx.NopLogger)
	if err := a.Err(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	built := time.Since(start)
	runtime.ReadMemStats(&total)
	exec := graphql.NewExecutor(s)
	r := exec.Execute(context.Background(), &graphql.Request{Query: `{ e000s(first: 1) { id } }`})
	if len(r.Errors) > 0 {
		fmt.Fprintln(os.Stderr, r.Errors)
		os.Exit(1)
	}
	retained := heap() - before
	fmt.Printf("build_ms=%.2f retained_kb=%d alloc_kb=%d mallocs=%d\\n",
		float64(built.Microseconds())/1000, retained/1024, (total.TotalAlloc-allocBefore)/1024, total.Mallocs-mallocsBefore)
	runtime.KeepAlive(a)
	runtime.KeepAlive(exec)
}
""")

subprocess.run([gqlc, "-config", "gqlc.yaml"], cwd=out, check=True, stderr=subprocess.DEVNULL)

if layout == "deps":
    lines = "".join(f"\t\t{e}.Bindings({e}.NewHandler(d)),\n" for e in names)
    write("graph/wire.go", f"""package graph

import (
	"github.com/syssam/graphql-go"

{graph_imports}	"scale/internal/service"
)

// Wire builds every group's Resolver from the whole dependencies struct.
func Wire(d service.Services) []graphql.SchemaOption {{
	return []graphql.SchemaOption{{
{lines}	}}
}}
""")

subprocess.run(["go", "mod", "tidy"], cwd=out, check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
