package graph

import (
	"context"
	"iter"
	"net/url"

	"github.com/bgfdev/taut/internal/fail"
)

// page is one page of an OData collection.
type page[T any] struct {
	Value    []T    `json:"value"`
	NextLink string `json:"@odata.nextLink"`
}

// Pages iterates an OData collection at path (for example "/v1.0/users"),
// following @odata.nextLink until the collection is exhausted. Pages are
// fetched lazily. On failure the iterator yields the zero T with the error
// and stops.
func Pages[T any](ctx context.Context, c *Client, path string, query url.Values) iter.Seq2[T, error] {
	return list[T](ctx, c, request{method: "GET", path: path, query: query})
}

func list[T any](ctx context.Context, c *Client, rq request) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		var zero T
		seen := map[string]bool{}
		for {
			var p page[T]
			if err := c.do(ctx, rq, &p); err != nil {
				yield(zero, err)
				return
			}
			for _, item := range p.Value {
				if !yield(item, nil) {
					return
				}
			}
			if p.NextLink == "" {
				return
			}
			if seen[p.NextLink] {
				yield(zero, fail.New("graph.bad_response",
					"Microsoft Graph returned a paging link that loops",
					"The same @odata.nextLink was returned twice for "+rq.path+".",
					"Run the command again. If it repeats, report it with --verbose output."))
				return
			}
			seen[p.NextLink] = true
			rq = request{method: "GET", absolute: p.NextLink, path: rq.path, permission: rq.permission}
		}
	}
}
