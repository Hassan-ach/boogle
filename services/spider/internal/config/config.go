package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
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
}

func LoadConfig() (*Config, error) {
	c := &Config{
		App:      loadAppConfig(),
		Store:    loadStoreConfig(),
		RabbitMq: loadRabbitMqConfig(),
	}

	fmt.Printf("%+v\n", c)

	return c, nil
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
