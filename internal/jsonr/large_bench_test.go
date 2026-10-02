package jsonr

import (
	"fmt"
	"strings"
	"testing"
)

// largeInputs are variables objects past shareLimit, where every string is
// copied out: a bulk mutation's list of input objects, and one large string.
func largeInputs() map[string][]byte {
	rows := func(n int) []byte {
		var sb strings.Builder
		sb.WriteString(`{"input":[`)
		for i := range n {
			if i > 0 {
				sb.WriteByte(',')
			}
			fmt.Fprintf(&sb, `{"sku":"sku-%05d","name":"Product number %d","priceCents":%d,"active":true,"note":null,"tags":["a","b","c"],"dims":{"w":1.5,"h":2.25,"unit":"cm"}}`, i, i, i*100)
		}
		sb.WriteString(`],"dryRun":false}`)
		return []byte(sb.String())
	}
	return map[string][]byte{
		"10 rows (1.4 KB)":     rows(10),
		"100 rows (14 KB)":     rows(100),
		"10000 rows (1.5 MB)":  rows(10000),
		"one 1 MB string":      []byte(`{"id":"abc","upload":"` + strings.Repeat("x", 1<<20) + `"}`),
		"escaped strings 14KB": []byte(strings.ReplaceAll(string(rows(100)), "Product number", `Product \"quoted\" é number`)),
	}
}

func BenchmarkLargeObject(b *testing.B) {
	for name, raw := range largeInputs() {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(raw)))
			for b.Loop() {
				if _, err := Object(raw); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkLargeEncodingJSON(b *testing.B) {
	for name, raw := range largeInputs() {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(raw)))
			for b.Loop() {
				if _, err := reference(raw); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
