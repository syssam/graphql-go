package graphql

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// maxBindingCode bounds the machine code this package's generics instantiate
// in a package binding one entity the way generated code does (bindGroupSrc).
// A 300-entity schema has 300 such packages, and one edit to a type they all
// import recompiles every one: the code instantiated here was 94% of what each
// compiled, and 48% of the whole rebuild (docs/large-schemas.md). Moving the
// build-time logic out of the generic bodies, into non-generic functions
// compiled once, took it from 212 KB to 109 KB (linux/amd64, go1.27). The
// bound is 8% over that: moving only the field composition back into
// newFieldSpec reaches 125 KB and fails. go.mod pins the toolchain, so the
// number moves only with a Go upgrade; re-measure and rebase it then, as for
// the allocation baseline.
const maxBindingCode = 118 << 10

// bindGroupSrc is one velox-shaped group: two enums bound through MarshalGQL
// (one string-shaped, one a struct holding a func, as an ORM's order field
// is), create and where inputs, an object with scalar, edge and connection
// fields, a connection and edge type, and root fields with arguments.
const bindGroupSrc = `package group

import (
	"context"
	"io"
	"strconv"
	"time"

	"github.com/syssam/graphql-go"
)

type Status string

func (s Status) MarshalGQL(w io.Writer)      { io.WriteString(w, strconv.Quote(string(s))) }
func (s *Status) UnmarshalGQL(v any) error   { *s = Status(v.(string)); return nil }

type OrderField struct{ column string; value func(*Item) any }

func (f OrderField) MarshalGQL(w io.Writer)    { io.WriteString(w, strconv.Quote(f.column)) }
func (f *OrderField) UnmarshalGQL(v any) error { f.column = v.(string); return nil }

type Item struct {
	ID        int
	Name      string
	Code      string
	Amount    int
	Active    bool
	Note      *string
	Status    Status
	CreatedAt time.Time
	parent    *Item
	children  *ItemConnection
}

func (i *Item) Parent(context.Context) (*Item, error) { return i.parent, nil }

type ItemEdge struct {
	Node   *Item
	Cursor string
}

type ItemConnection struct {
	Edges      []*ItemEdge
	TotalCount int
}

type ItemOrder struct {
	Field     *OrderField
	Direction string
}

type CreateItemInput struct {
	Name     string
	Code     string
	Amount   *int
	Active   *bool
	Note     *string
	Status   *Status
	ParentID *int
}

type ItemWhereInput struct {
	Not           *ItemWhereInput
	And           []*ItemWhereInput
	Or            []*ItemWhereInput
	Name          *string
	NameIn        []string
	NameContains  *string
	Amount        *int
	AmountGT      *int
	AmountIn      []int
	Status        *Status
	StatusIn      []Status
	Active        *bool
	HasParent     *bool
	HasParentWith []*ItemWhereInput
}

type ItemsArgs struct {
	First   *int
	After   *string
	Where   *ItemWhereInput
	OrderBy *ItemOrder
}

type ChildrenArgs struct {
	First *int
	After *string
	Where *ItemWhereInput
}

type CreateItemArgs struct{ Input CreateItemInput }

type UpdateItemArgs struct {
	ID    int
	Input CreateItemInput
}

type Resolver interface {
	Items(context.Context, ItemsArgs) (*ItemConnection, error)
	CreateItem(context.Context, CreateItemArgs) (*Item, error)
	UpdateItem(context.Context, UpdateItemArgs) (*Item, error)
}

func Bindings(r Resolver) graphql.SchemaOption {
	return graphql.Options(
		graphql.EnumMarshaler[Status]("ItemStatus"),
		graphql.EnumMarshaler[OrderField]("ItemOrderField"),
		graphql.Input[CreateItemInput]("CreateItemInput", graphql.ZeroForNull()),
		graphql.Input[ItemWhereInput]("ItemWhereInput", graphql.ZeroForNull()),
		graphql.Input[ItemOrder]("ItemOrder", graphql.ZeroForNull()),
		graphql.Object[Item]("Item",
			graphql.Field("id", func(v *Item) int { return v.ID }),
			graphql.Field("name", func(v *Item) string { return v.Name }),
			graphql.Field("code", func(v *Item) string { return v.Code }),
			graphql.Field("amount", func(v *Item) int { return v.Amount }),
			graphql.Field("active", func(v *Item) bool { return v.Active }),
			graphql.Field("note", func(v *Item) *string { return v.Note }),
			graphql.Field("status", func(v *Item) Status { return v.Status }),
			graphql.Field("createdAt", func(v *Item) time.Time { return v.CreatedAt }),
			graphql.Resolve("parent", func(ctx context.Context, v *Item) (*Item, error) { return v.Parent(ctx) }, graphql.Inline()),
			graphql.ResolveArgs("children", func(ctx context.Context, v *Item, a ChildrenArgs) (*ItemConnection, error) {
				return v.children, nil
			}),
		),
		graphql.Object[ItemConnection]("ItemConnection",
			graphql.Field("edges", func(v *ItemConnection) []*ItemEdge { return v.Edges }),
			graphql.Field("totalCount", func(v *ItemConnection) int { return v.TotalCount }),
		),
		graphql.Object[ItemEdge]("ItemEdge",
			graphql.Field("node", func(v *ItemEdge) *Item { return v.Node }),
			graphql.Field("cursor", func(v *ItemEdge) string { return v.Cursor }),
		),
		graphql.Query(
			graphql.ResolveArgs("items", func(ctx context.Context, _ graphql.Root, a ItemsArgs) (*ItemConnection, error) {
				return r.Items(ctx, a)
			}),
		),
		graphql.Mutation(
			graphql.ResolveArgs("createItem", func(ctx context.Context, _ graphql.Root, a CreateItemArgs) (*Item, error) {
				return r.CreateItem(ctx, a)
			}),
			graphql.ResolveArgs("updateItem", func(ctx context.Context, _ graphql.Root, a UpdateItemArgs) (*Item, error) {
				return r.UpdateItem(ctx, a)
			}),
		),
		graphql.Args[ItemsArgs](),
		graphql.Args[ChildrenArgs](),
		graphql.Args[CreateItemArgs](),
		graphql.Args[UpdateItemArgs](),
	)
}
`

func TestBindingCodeSize(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping go build subprocess")
	}
	_, thisFile, _, _ := runtime.Caller(0)
	root := filepath.Dir(thisFile)
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sum, err := os.ReadFile(filepath.Join(root, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	write("go.sum", string(sum))
	write("go.mod", "module bindsize\n\ngo 1.27\n\nrequire github.com/syssam/graphql-go v0.0.0\n\nrequire github.com/vektah/gqlparser/v2 v2.5.37\n\nreplace github.com/syssam/graphql-go => "+filepath.ToSlash(root)+"\n")
	write("group/group.go", bindGroupSrc)

	run := func(args ...string) []byte {
		t.Helper()
		cmd := exec.Command("go", args...)
		cmd.Dir = dir
		// The bound is in amd64 bytes; cross-compiling measures the same
		// code on any host.
		cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
		out, err := cmd.Output()
		if err != nil {
			var stderr []byte
			if ee, ok := err.(*exec.ExitError); ok {
				stderr = ee.Stderr
			}
			t.Fatalf("go %s: %v\n%s", strings.Join(args, " "), err, stderr)
		}
		return out
	}
	archive := strings.TrimSpace(string(run("list", "-mod=mod", "-export", "-f", "{{.Export}}", "./group")))
	nm := run("tool", "nm", "-size", archive)

	var total, lib int
	sc := bufio.NewScanner(bytes.NewReader(nm))
	for sc.Scan() {
		// address size type name
		f := strings.Fields(sc.Text())
		if len(f) < 4 || (f[2] != "T" && f[2] != "t") {
			continue
		}
		n, err := strconv.Atoi(f[1])
		if err != nil {
			continue
		}
		total += n
		if strings.HasPrefix(f[3], "github.com/syssam/graphql-go.") {
			lib += n
		}
	}
	t.Logf("group package: %d bytes of code, %d instantiated from graphql-go (bound %d)", total, lib, maxBindingCode)
	if lib == 0 {
		t.Fatalf("no graphql-go code found in the archive; the measurement is broken:\n%s", nm)
	}
	if lib > maxBindingCode {
		t.Errorf("one generated-shaped group instantiates %d bytes of graphql-go code, over %d: a generic binding function is carrying build-time logic every package compiles again (see maxBindingCode)", lib, maxBindingCode)
	}
}
