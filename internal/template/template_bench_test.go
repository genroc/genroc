package template

import "testing"

// Evidence for the `cache` package var: on an M1 Parse costs 630ns/8 allocs to 3.2us/38 allocs
// against ~12ns and zero allocs cached. Re-run before proposing the cache be removed.
var benchSrcs = []string{
	`${ input.order_id }`,
	`https://api.example.com/v1/orders/${ input.order_id }/items?since=${ outputs.fetch.cursor }`,
	`Order ${ input.order_id } for ${ input.customer.name } totalling ${ outputs.total.amount } at ${ outputs.total.currency }`,
}

func BenchmarkParse(b *testing.B) {
	for _, src := range benchSrcs {
		b.Run(src[:min(len(src), 24)], func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := Parse(src); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkGet(b *testing.B) {
	for _, src := range benchSrcs {
		if _, err := Get(src); err != nil {
			b.Fatalf("warm the cache: %v", err)
		}
		b.Run(src[:min(len(src), 24)], func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := Get(src); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
