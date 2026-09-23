package codegen

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A Go constant's identifier says nothing about which SDL value it carries;
// its value says exactly. These tests pin the discovery that reads the value,
// and one of them compiles the result -- which is the only thing that would
// have caught the bug: the generated file reads perfectly well while naming a
// constant that does not exist.

const enumSDL = `
enum AccessPolicy { EXPECT_VISIBLE HIDDEN }
enum OrderField { WORKSPACE_ID CREATED_AT }
type Doc {
  id: ID!
  policy: AccessPolicy!
  orderBy: OrderField!
}
type Query { doc(id: ID!): Doc }
`

// enumSource names its constants the way a real ORM does, which is not the way
// the generator would derive them: AccessPolicyExpectVisible where the
// derivation gives AccessPolicyExpect_visible, WorkspaceID where it gives
// Workspace_id.
const enumSource = `package ent

type AccessPolicy string

const (
	AccessPolicyExpectVisible AccessPolicy = "EXPECT_VISIBLE"
	AccessPolicyHidden        AccessPolicy = "HIDDEN"
)

type OrderField string

const (
	OrderFieldWorkspaceID OrderField = "WORKSPACE_ID"
	OrderFieldCreatedAt   OrderField = "CREATED_AT"
)

type Doc struct {
	ID      string
	Policy  AccessPolicy
	OrderBy OrderField
}
`

func TestAutoBindDiscoversEnumConstants(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping package load")
	}
	dir := writeEntModule(t, enumSDL, enumSource)
	// AccessPolicy is declared in Models, OrderField is only discovered, so
	// both paths into the constant index are exercised at once.
	src := autoGenerate(t, dir, Config{Models: map[string]string{"AccessPolicy": "hello/ent.AccessPolicy"}})

	// gofmt aligns the map literal, so compare with runs of spaces collapsed.
	flat := strings.Join(strings.Fields(src), " ")
	for _, want := range []string{
		"ent.AccessPolicyExpectVisible: \"EXPECT_VISIBLE\"",
		"ent.AccessPolicyHidden: \"HIDDEN\"",
		"ent.OrderFieldWorkspaceID: \"WORKSPACE_ID\"",
		"ent.OrderFieldCreatedAt: \"CREATED_AT\"",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("generated code does not bind %s\n%s", want, src)
		}
	}
	for _, bad := range []string{"AccessPolicyExpect_visible", "OrderFieldWorkspace_id"} {
		if strings.Contains(src, bad) {
			t.Errorf("generated code still derives %s", bad)
		}
	}

	// A discovered enum is a model the generator no longer writes, and with
	// every type in this schema bound there is no model package at all.
	if _, err := os.Stat(filepath.Join(dir, "graph", "model")); !os.IsNotExist(err) {
		t.Errorf("a model package was written for a fully bound schema: %v", err)
	}

	// Discovery binds the enum before it decides the fields typed by it, so
	// Doc.orderBy is a struct field and not a resolver.
	if strings.Contains(src, "DocOrderBy(") {
		t.Errorf("orderBy fell through to the resolver\n%s", src)
	}

	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, out)
	}
	build := exec.Command("go", "build", "./graph/...")
	build.Dir = dir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
}

// Half a discovery is worse than none for an enum the generator would
// otherwise model itself: the values it did find would name constants in the
// package it writes, where they do not exist.
func TestPartialEnumDiscoveryBindsNothing(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping package load")
	}
	const sdl = `
enum OrderField { WORKSPACE_ID CREATED_AT DELETED_AT }
type Doc { id: ID! orderBy: OrderField! }
type Query { doc(id: ID!): Doc }
`
	const source = `package ent

type OrderField string

const (
	OrderFieldWorkspaceID OrderField = "WORKSPACE_ID"
	OrderFieldCreatedAt   OrderField = "CREATED_AT"
)

type Doc struct {
	ID      string
	OrderBy OrderField
}
`
	dir := writeEntModule(t, sdl, source)
	src := autoGenerate(t, dir, Config{})
	if strings.Contains(src, "ent.OrderFieldWorkspaceID") {
		t.Errorf("bound an enum whose DELETED_AT has no constant\n%s", src)
	}
	model, err := os.ReadFile(filepath.Join(dir, "graph", "model", "models.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(model), "type OrderField") {
		t.Error("no model generated for an enum that was not bound")
	}
}

// An ORM declares an enum's Go type beside the entity that uses it, so the
// packages holding them are named by the model map and not by AutoBind. One
// real schema names about five hundred of them; requiring the author to list
// each one a second time is bookkeeping, and getting it wrong is silent.
func TestEnumConstantsComeFromTheModelMapsPackages(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping package load")
	}
	const sdl = `
enum Status { EXPECT_OPEN CLOSED }
type Doc { id: ID! status: Status! }
type Query { doc(id: ID!): Doc }
`
	const source = `package ent

import "hello/statuspkg"

type Doc struct {
	ID     string
	Status statuspkg.Status
}
`
	dir := writeEntModule(t, sdl, source)
	if err := os.MkdirAll(filepath.Join(dir, "statuspkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	const statusSrc = `package statuspkg

type Status string

const (
	StatusExpectOpen Status = "EXPECT_OPEN"
	StatusClosed     Status = "CLOSED"
)
`
	if err := os.WriteFile(filepath.Join(dir, "statuspkg", "status.go"), []byte(statusSrc), 0o644); err != nil {
		t.Fatal(err)
	}

	// statuspkg is named only by Models; AutoBind still points at ./ent.
	src := autoGenerate(t, dir, Config{Models: map[string]string{"Status": "hello/statuspkg.Status"}})
	flat := strings.Join(strings.Fields(src), " ")
	for _, want := range []string{
		"statuspkg.StatusExpectOpen: \"EXPECT_OPEN\"",
		"statuspkg.StatusClosed: \"CLOSED\"",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("not bound: %s\n%s", want, src)
		}
	}
	if strings.Contains(src, "StatusExpect_open") {
		t.Error("the constant name was derived rather than read")
	}

	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, out)
	}
	build := exec.Command("go", "build", "./graph/...")
	build.Dir = dir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
}

// ent stores an enum column lower-case and upper-cases it on the way out, so
// EventTypeView = "view" is the constant for the SDL value VIEW. An exact
// match finds nothing for an entire ORM's worth of enums; an ambiguous fold
// must still find nothing, because guessing there binds the wrong constant
// silently.
func TestEnumConstantsMatchCaseInsensitively(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping package load")
	}
	const sdl = `
enum EventType { VIEW CLICK }
enum Ambiguous { READY }
type Doc { id: ID! event: EventType! state: Ambiguous! }
type Query { doc(id: ID!): Doc }
`
	const source = `package ent

type EventType string

const (
	EventTypeView  EventType = "view"
	EventTypeClick EventType = "click"
)

type Ambiguous string

const (
	AmbiguousLower Ambiguous = "ready"
	AmbiguousTitle Ambiguous = "Ready"
)

type Doc struct {
	ID    string
	Event EventType
	State Ambiguous
}
`
	dir := writeEntModule(t, sdl, source)
	src := autoGenerate(t, dir, Config{})
	flat := strings.Join(strings.Fields(src), " ")
	for _, want := range []string{
		"ent.EventTypeView: \"VIEW\"",
		"ent.EventTypeClick: \"CLICK\"",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("not bound: %s\n%s", want, src)
		}
	}
	// READY has two constants differing only in case and neither spelling it
	// exactly, so nothing is bound for Ambiguous and the generator models it.
	if strings.Contains(src, "ent.Ambiguous") {
		t.Errorf("an ambiguous fold was resolved anyway\n%s", src)
	}

	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	if out, err := tidy.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy: %v\n%s", err, out)
	}
	build := exec.Command("go", "build", "./graph/...")
	build.Dir = dir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
}

// An exact match must still win a fold: a type carrying both "VIEW" and
// "view" answers VIEW with the one that says so.
func TestExactEnumValueBeatsTheFold(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping package load")
	}
	const sdl = `
enum EventType { VIEW }
type Doc { id: ID! event: EventType! }
type Query { doc(id: ID!): Doc }
`
	const source = `package ent

type EventType string

const (
	EventTypeExact EventType = "VIEW"
	EventTypeLower EventType = "view"
)

type Doc struct {
	ID    string
	Event EventType
}
`
	dir := writeEntModule(t, sdl, source)
	src := autoGenerate(t, dir, Config{})
	if !strings.Contains(strings.Join(strings.Fields(src), " "), "ent.EventTypeExact: \"VIEW\"") {
		t.Errorf("the exact value did not win\n%s", src)
	}
}
