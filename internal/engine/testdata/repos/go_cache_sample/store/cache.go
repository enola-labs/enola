package store

import "context"

// ReadThrough keeps the fetch function its constructor is handed and calls it on a miss.
type ReadThrough struct {
	source func(ctx context.Context, ids []int) (map[int]string, error)
}

func NewReadThrough(source func(ctx context.Context, ids []int) (map[int]string, error)) *ReadThrough {
	return &ReadThrough{source: source}
}

func (r *ReadThrough) GetMany(ctx context.Context, ids []int) (map[int]string, error) {
	return r.source(ctx, ids)
}
