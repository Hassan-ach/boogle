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
	conn *redis.Client
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

	return &RedisClient{conn: client}
}

func (c *RedisClient) Close() {
	_ = c.conn.Close()
}

// Conn exposes the underlying connection so the policy manager can share this
// pool rather than opening a second one against the same server.
//
// It is here, on the concrete type, and not on the Cache interface, because
// nothing that consumes a Cache should be handed the ability to run arbitrary
// commands against it. The policy manager genuinely needs this -- it owns the
// frontier and the visited set, which are policy state -- but it needs one
// connection, not the store's whole surface.
func (c *RedisClient) Conn() *redis.Client {
	return c.conn
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
