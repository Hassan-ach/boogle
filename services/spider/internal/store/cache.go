package store

import (
	"context"
	"log"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Hassan-ach/boogle/services/spider/internal/config"
)

type RedisClient struct {
	conn *redis.Client
}

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

	return &RedisClient{conn: client}
}

func (c *RedisClient) Close() {
	_ = c.conn.Close()
}

// Conn is the shared connection policy.RedisState reads and writes host state
// through. One pool for both, so a spider does not hold two sets of idle
// connections to the same server.
func (c *RedisClient) Conn() *redis.Client {
	return c.conn
}
