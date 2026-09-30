package config

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/Hassan-ach/boogle/services/spider/internal/policy"
)

type RabbitMqConfig struct {
	Protocol string
	User     string
	Password string
	Host     string
	Port     int
}

type RedisConfig struct {
	Addr     string
	Password string
	DB       int
	Port     int
	// Delay and MaxRetry are dead. They configured the old frontier pop loop,
	// which conflated a scan budget with a retry count and burned up to ten
	// frontier entries per call. They are still read so an existing deployment
	// does not fail to start, and they are read by nothing. Phases that consume
	// FRONTIER_POP_BATCH and FRONTIER_POP_RETRY_MS remove them.
	Delay    int
	MaxRetry int
}

type PSQLConfig struct {
	Host     string
	Port     int
	User     string
	DBname   string
	Password string

	MaxOpenConns    int
	MaxIdleConns    int
	MaxConnLifetime time.Duration

	BatchSize int
}

type StoreConfig struct {
	Cache RedisConfig
	DB    PSQLConfig
}

type AppConfig struct {
	MaxCrawlers        int
	MaxConcurrentFetch int

	LogsPath string

	ClawlerDelay int

	HttpTimeout    int
	CrawlerTimeout int
}

type Config struct {
	App      AppConfig
	Store    StoreConfig
	RabbitMq RabbitMqConfig
	Policy   policy.Config
}

func LoadConfig() (*Config, error) {
	c := &Config{
		App:      loadAppConfig(),
		Store:    loadStoreConfig(),
		RabbitMq: loadRabbitMqConfig(),
		Policy:   loadPolicyConfig(),
	}

	fmt.Printf("%+v\n", c)

	return c, nil
}

// loadPolicyConfig builds the policy manager's configuration from the
// environment, starting from the package's own defaults.
//
// Starting from DefaultConfig rather than from a zero struct is the important
// part: every field has a defensible value with no environment set at all, so a
// missing variable is a default rather than a zero. A zero here is a silent
// failure -- a zero batch size means "scan nothing", and a zero page budget means
// "crawl no pages" -- and both look exactly like a crawl that has finished.
func loadPolicyConfig() policy.Config {
	c := policy.DefaultConfig()
	c.RedisPrefix = getWithDefault("SPIDER_REDIS_PREFIX", c.RedisPrefix)
	c.URLStateTTL = secondsWithDefault("URL_STATE_TTL_SEC", c.URLStateTTL)
	c.FrontierPopBatch = getIntWithDefault("FRONTIER_POP_BATCH", c.FrontierPopBatch)
	c.DelayedPromoteBatch = getIntWithDefault("DELAYED_PROMOTE_BATCH", c.DelayedPromoteBatch)
	return c
}

// secondsWithDefault reads a duration expressed as whole seconds, falling back on
// anything that is not a positive number.
//
// A malformed or non-positive value falls back rather than being taken literally,
// because both zero and negative are silent failure modes here: a zero TTL expires
// every URL's retry bookkeeping on the next read, which turns the backoff off
// without saying so.
func secondsWithDefault(key string, fallback time.Duration) time.Duration {
	raw := getWithDefault(key, "")
	if raw == "" {
		return fallback
	}
	secs, err := strconv.Atoi(raw)
	if err != nil || secs <= 0 {
		fmt.Printf("%s=%q is not a positive number of seconds; using %s\n", key, raw, fallback)
		return fallback
	}
	return time.Duration(secs) * time.Second
}

func loadRabbitMqConfig() RabbitMqConfig {

	protocol := getWithDefault("RABBITMQ_PROTOCOL", "amqp")
	user := getWithDefault("RABBITMQ_USER", "guest")
	password := getWithDefault("RABBITMQ_PASSWORD", "guest")
	host := getWithDefault("RABBITMQ_HOST", "localhost")
	port := getIntWithDefault("RABBITMQ_PORT", 5672)

	return RabbitMqConfig{
		protocol,
		user,
		password,
		host,
		port,
	}
}

func loadStoreConfig() StoreConfig {
	return StoreConfig{
		Cache: loadRedisConfig(),
		DB:    loadDatabaseConfig(),
	}
}

func loadDatabaseConfig() PSQLConfig {
	host := getWithDefault("PG_HOST", "localhost")
	port := getIntWithDefault("PG_PORT", 5432)
	user := getWithDefault("PG_USER", "admin")
	password := getWithDefault("PG_PASSWORD", "1234")
	dbname := getWithDefault("PG_DBNAME", "se")
	maxOpenConns := getIntWithDefault("PG_MAX_OPEN_CONNS", 20)
	maxIdleConns := getIntWithDefault("PG_MAX_IDLE_CONNS", 20)
	maxConnLifetime := getIntWithDefault("PG_MAX_CONN_LIFETIME", 0)
	batchSize := getIntWithDefault("PG_BATCH_SIZE", 30)

	return PSQLConfig{
		Host:            host,
		Port:            port,
		User:            user,
		DBname:          dbname,
		Password:        password,
		MaxOpenConns:    maxOpenConns,
		MaxIdleConns:    maxIdleConns,
		MaxConnLifetime: time.Second * time.Duration(maxConnLifetime),
		BatchSize:       batchSize,
	}
}

func loadRedisConfig() RedisConfig {
	addr := getWithDefault("REDIS_ADDR", "localhost")
	password := getWithDefault("REDIS_PASSWORD", "")
	db := getIntWithDefault("REDIS_DB", 1)
	port := getIntWithDefault("REDIS_PORT", 6379)
	delay := getIntWithDefault("REDIS_DELAY", 5)
	maxRetry := getIntWithDefault("REDIS_MAX_RETRY", 10)

	return RedisConfig{
		Addr:     addr,
		Password: password,
		Port:     port,
		DB:       db,
		Delay:    delay,
		MaxRetry: maxRetry,
	}
}

func loadAppConfig() AppConfig {
	maxCrawlers := getIntWithDefault("MAX_CRAWLERS", 20)
	httpTimeout := getIntWithDefault("HTTP_TIMEOUT", 60)
	crawlerTimeout := getIntWithDefault("CRAWLER_TIMEOUT", 60)
	maxConcurrentFetch := getIntWithDefault("MAX_CONCURRENT_FETCH", 200)
	logsPath := getWithDefault("LOGS_PATH", "./logs.json")
	clawlerDelay := getIntWithDefault("CRAWLER_DELAY", 200)
	return AppConfig{
		MaxCrawlers:        maxCrawlers,
		CrawlerTimeout:     crawlerTimeout,
		HttpTimeout:        httpTimeout,
		MaxConcurrentFetch: maxConcurrentFetch,
		LogsPath:           logsPath,
		ClawlerDelay:       clawlerDelay,
	}
}

func getWithDefault(key, defaultValue string) string {
	k := os.Getenv(key)
	if k == "" {
		return defaultValue
	}
	return k
}

func getIntWithDefault(key string, defaultValue int) int {
	k := getWithDefault(key, "")
	v, err := strconv.Atoi(k)
	if err != nil {
		return defaultValue
	}
	return v
}
