package shelf

import (
	"context"

	"example.com/shelf/store"
)

type catalog interface {
	Titles(ctx context.Context, ids []int) (map[int]string, error)
}

type titlesCache struct {
	rt *store.ReadThrough
}

func newTitlesCache(api catalog) *titlesCache {
	fetch := func(ctx context.Context, ids []int) (map[int]string, error) {
		return api.Titles(ctx, ids)
	}
	return &titlesCache{rt: store.NewReadThrough(fetch)}
}

func (c *titlesCache) get(ctx context.Context, ids []int) (map[int]string, error) {
	return c.rt.GetMany(ctx, ids)
}
