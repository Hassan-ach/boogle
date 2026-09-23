package store

import (
	"bytes"
	"context"
	"encoding/gob"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Hassan-ach/boogle/services/spider/internal/config"
	"github.com/Hassan-ach/boogle/services/spider/internal/entity"
)

type RedisClient struct {
	conn     *redis.Client
	delay    int
	maxRetry int
}

// NewRedisClient initializes and returns a Redis client and wrapper.
// Registers entity.Host type with gob for serialization.
func NewRedisClient(conf config.RedisConfig) *RedisClient {
	port := strconv.Itoa(conf.Port)
	client := redis.NewClient(&redis.Options{
		Addr:         conf.Addr + ":" + port,
		Password:     conf.Password,
		DB:           conf.DB,
		Protocol:     2,
		DialTimeout:  5 * time.Second,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 5 * time.Second,
	})

	_, err := client.Ping(context.Background()).Result()
	if err != nil {
		log.Fatalf("Failed to connected to redis ERROR: %v", err)
	}

	// Required for gob encoding/decoding of Host structs
	gob.Register(entity.Host{})
	fmt.Println("Cache Connected")

	return &RedisClient{
		conn:     client,
		delay:    conf.Delay,
		maxRetry: conf.MaxRetry,
	}
}

func (c *RedisClient) Close() {
	_ = c.conn.Close()
}

// AddHostMetaData serializes a Host struct with gob and stores it in Redis.
func (c *RedisClient) AddHostMetaData(ctx context.Context, h string, host *entity.Host) error {
	if h == "" || host == nil {
		return fmt.Errorf("invalid host metadata: host key and Host struct cannot be empty")
	}

	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(host); err != nil {
		return fmt.Errorf("encode metadata: %w", err)
	}

	err := c.conn.HSet(ctx, "hosts", h, buf.Bytes()).Err()
	if err != nil {
		return fmt.Errorf("store metadata: %w", err)
	}

	return nil
}

// GetHostMetaData retrieves and decodes a Host struct from Redis.
func (c *RedisClient) GetHostMetaData(ctx context.Context, h string) (*entity.Host, bool, error) {
	val, err := c.conn.HGet(ctx, "hosts", h).Bytes()
	if err == redis.Nil {
		return nil, false, nil
	}

	if err != nil {
		return nil, false, fmt.Errorf("retrieve host metadata: %w", err)
	}

	var host entity.Host
	if err := gob.NewDecoder(bytes.NewReader(val)).Decode(&host); err != nil {
		return nil, false, fmt.Errorf("decode host metadata: %w", err)
	}

	return &host, true, nil
}

func (c *RedisClient) GetUrl(ctx context.Context) (string, bool, error) {
	script := redis.NewScript(`
	local res = redis.call("zpopmax", KEYS[1])
	if not res[1] then
		return false
	end
	local url = res[1]
	local score = res[2]

	if redis.call("sismember", KEYS[2], url) == 1 then
		return false
	end

	return url
	`)

	var err error
	var val any
	for i := 0; i < c.maxRetry; i++ {
		val, err = script.Run(ctx, c.conn, []string{"urls", "visitedUrls"}).Result()
		if err != nil {
			// on error, wait and retry
			time.Sleep(10 * time.Millisecond)
			continue
		}

		if valStr, ok := val.(string); ok && valStr != "" {
			return valStr, true, nil
		} else {
			err = fmt.Errorf("script returned non-string or empty value: %v", val)
		}

		time.Sleep(time.Duration(c.delay) * time.Millisecond)
	}

	return "", false, fmt.Errorf("no valid URL after %d retries err: %w", c.maxRetry, err)
}

func (c *RedisClient) AddUrls(ctx context.Context, urls []string) error {
	if len(urls) == 0 {
		return nil
	}

	pipe := c.conn.Pipeline()

	cmds := make([]*redis.BoolCmd, len(urls))
	for i, u := range urls {
		cmds[i] = pipe.SIsMember(ctx, "visitedUrls", u)
	}

	if _, err := pipe.Exec(ctx); err != nil {
		return err
	}

	pipe = c.conn.Pipeline()

	for i, u := range urls {
		visited, err := cmds[i].Result()
		if err != nil {
			return err
		}

		if !visited {
			pipe.ZIncrBy(ctx, "urls", 1, u)
		}
	}

	_, err := pipe.Exec(ctx)
	return err
}

// AddToVisitedUrl adds a URL to the visitedUrls set.
func (c *RedisClient) MarkVisited(ctx context.Context, u string) error {
	if u == "" {
		return nil
	}

	err := c.conn.SAdd(ctx, "visitedUrls", u).Err()
	if err != nil {
		return fmt.Errorf("add to visited URLs: %w", err)
	}

	return nil
}

// AddToWaitedHost adds a host key in Redis with a TTL corresponding to delay.
func (c *RedisClient) AddToWaitedHost(ctx context.Context, h string, delay int) error {
	if h == "" {
		return fmt.Errorf("empty host cannot be added to waited hosts")
	}

	err := c.conn.Set(ctx, h, 1, time.Duration(delay)*time.Second).Err()
	if err != nil {
		return fmt.Errorf("add to waited host: %w", err)
	}
	return nil
}

// CountUrls returns the number of URLs in the sorted set.
func (c *RedisClient) CountUrls(ctx context.Context) int64 {
	count, err := c.conn.ZCard(ctx, "urls").Result()
	if err != nil {
		return 0
	}
	return count
}
