// Package cache Redis sarmalayicisi. REDIS_URL bos ise devre disi kalir
// (tum metodlar guvenle no-op olur), boylece backend Redis olmadan da calisir.
package cache

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

type Cache struct {
	rdb *redis.Client
}

// New REDIS_URL'den baglanir. Bos url -> devre disi Cache (hata dondurmez).
// Ornek url: redis://:sifre@redis:6379/0
func New(ctx context.Context, url string) (*Cache, error) {
	if url == "" {
		return &Cache{}, nil
	}
	opt, err := redis.ParseURL(url)
	if err != nil {
		return nil, err
	}
	rdb := redis.NewClient(opt)
	if err := rdb.Ping(ctx).Err(); err != nil {
		return nil, err
	}
	return &Cache{rdb: rdb}, nil
}

func (c *Cache) Enabled() bool { return c != nil && c.rdb != nil }

func (c *Cache) Close() {
	if c.Enabled() {
		_ = c.rdb.Close()
	}
}

// GetString anahtari okur; yoksa veya hata varsa ("", false).
func (c *Cache) GetString(ctx context.Context, key string) (string, bool) {
	if !c.Enabled() {
		return "", false
	}
	v, err := c.rdb.Get(ctx, key).Result()
	if err != nil {
		return "", false
	}
	return v, true
}

func (c *Cache) SetString(ctx context.Context, key, val string, ttl time.Duration) {
	if !c.Enabled() {
		return
	}
	_ = c.rdb.Set(ctx, key, val, ttl).Err()
}

func (c *Cache) Del(ctx context.Context, keys ...string) {
	if !c.Enabled() {
		return
	}
	_ = c.rdb.Del(ctx, keys...).Err()
}

// Incr anahtari 1 artirir; ilk artista [ttl] uygular. (yeni deger, true) doner.
// Cache devre disi ise (0, false) doner — cagiran in-memory yedege duser.
func (c *Cache) Incr(ctx context.Context, key string, ttl time.Duration) (int64, bool) {
	if !c.Enabled() {
		return 0, false
	}
	n, err := c.rdb.Incr(ctx, key).Result()
	if err != nil {
		return 0, false
	}
	if n == 1 {
		_ = c.rdb.Expire(ctx, key, ttl).Err()
	}
	return n, true
}

// GetInt anahtari sayi olarak okur; yoksa (0, false).
func (c *Cache) GetInt(ctx context.Context, key string) (int64, bool) {
	if !c.Enabled() {
		return 0, false
	}
	n, err := c.rdb.Get(ctx, key).Int64()
	if err != nil {
		return 0, false
	}
	return n, true
}
