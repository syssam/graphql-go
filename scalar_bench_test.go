package graphql

import (
	"context"
	"io"
	"testing"
)

type benchStamp string

type benchCode struct{ s string }

func (c benchCode) MarshalGQL(w io.Writer) {
	io.WriteString(w, `"`)
	io.WriteString(w, c.s)
	io.WriteString(w, `"`)
}

func (c *benchCode) UnmarshalGQL(v any) error {
	c.s, _ = v.(string)
	return nil
}

// A list of custom scalars is where the output check on Scalar and
// ScalarMarshaler shows: each value written is read once more to see that it
// is one JSON value. A thousand 36-byte values, the size of a UUID, through
// each binding.
func BenchmarkCustomScalarList(b *testing.B) {
	stamps := make([]benchStamp, 1000)
	codes := make([]benchCode, 1000)
	for i := range stamps {
		stamps[i] = "123e4567-e89b-12d3-a456-426614174000"
		codes[i] = benchCode{"123e4567-e89b-12d3-a456-426614174000"}
	}
	s, err := NewSchema(SDL(`scalar Stamp scalar Code type Query { stamps: [Stamp!]! codes: [Code!]! }`),
		Scalar("Stamp", func(w *Writer, v benchStamp) error { w.String(string(v)); return nil },
			func(v any) (benchStamp, error) { s, _ := v.(string); return benchStamp(s), nil }),
		ScalarMarshaler[benchCode]("Code"),
		Query(
			Field("stamps", func(Root) []benchStamp { return stamps }),
			Field("codes", func(Root) []benchCode { return codes }),
		),
	)
	if err != nil {
		b.Fatal(err)
	}
	e := NewExecutor(s)
	for name, query := range map[string]string{"Scalar": `{ stamps }`, "ScalarMarshaler": `{ codes }`} {
		b.Run(name, func(b *testing.B) {
			req := &Request{Query: query}
			b.ReportAllocs()
			for b.Loop() {
				resp := e.Execute(context.Background(), req)
				if len(resp.Errors) > 0 {
					b.Fatal(resp.Errors[0])
				}
				resp.Release()
			}
		})
	}
}
