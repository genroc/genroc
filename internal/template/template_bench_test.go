package template

import "testing"

// Evidence for the `cache` package var: Get is the runtime path (shape.Eval resolves
// every templated leaf through it on every advance), so if Parse were cheap the cache
// would not be worth a global. On an M1 it is not close — Parse costs 630ns/8 allocs for
// a bare expression up to 3.2us/38 allocs for a four-chunk string, against ~12ns and
// ZERO allocations cached. Re-run before proposing the cache be removed.
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
