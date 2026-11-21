package session

import (
	"context"

	"github.com/redis/go-redis/v9"
)

func Lookup(ctx context.Context, c *redis.Client, key string) string {
	v, _ := c.Get(ctx, key).Result()
	return v
}
