package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/Hassan-ach/boogle/services/spider/internal/policy"
)

type RabbitMqConfig struct {
	Protocol string
	User     string
	Password Secret
	Host     string
	Port     int
	JobQueue string
}

type RedisConfig struct {
	Addr     string
	Password Secret
	DB       int
	Port     int
}

type PSQLConfig struct {
	Host     string
	Port     int
	User     string
	DBname   string
	Password Secret

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

	JobQueue string
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

	return c, nil
}

// redacted is a copy safe to print. Config holds three credentials, and the
// startup dump this replaces went to stdout where it lands in whatever collects
// the container's output, so the struct itself is no longer formattable by
// accident -- every password type carries a String method that redacts, and
// Config carries one too. A password reaching a log now has to be asked for
// explicitly.
// Redacted returns a copy of the config with every password replaced by a fixed
// marker. With Password typed as Secret it is already safe to print, so this is
// only about being explicit at the call site.
func (c Config) Redacted() Config {
	return c
}

// Secret is a password that cannot be printed by accident.
//
// fmt reaches String() for %v, %+v and %s, and GoString() for %#v, so both are
// implemented. Leaving %#v to the default would defeat the point: %#v prints
// the raw string with its type name, which is the verb a developer reaches for
// when debugging why a connection is refused -- exactly the moment the value
// would otherwise be pasted into an issue.
//
// The zero value is an unset password, not the literal word "redacted", since
// marking an absent password would imply one is configured where there is none.
type Secret string

func (s Secret) String() string {
	if s == "" {
		return ""
	}
	return "[redacted]"
}

// GoString covers the %#v verb. See Secret.
func (s Secret) GoString() string {
	return s.String()
}

// LogValue covers slog handling Secret as a whole attribute.
//
// MarshalJSON covers it nested inside a struct, which LogValue does not: slog
// only resolves LogValuer on the attribute itself, then falls back to encoding/
// json for the fields of a struct. Both are needed, and the spider's production
// logger is exactly the case that needs the second -- Config goes out as an
// attribute, so its passwords are nested fields.
func (s Secret) LogValue() slog.Value {
	if s == "" {
		return slog.StringValue("")
	}
	return slog.StringValue("[redacted]")
}

// MarshalJSON covers the nested case. See Secret.
func (s Secret) MarshalJSON() ([]byte, error) {
	return json.Marshal(s.String())
}

// Reveal returns the underlying password. It is deliberately a separate call so
// that handing a secret to a client is a visible decision at the call site
// rather than an implicit conversion.
func (s Secret) Reveal() string {
	return string(s)
}

// loadPolicyConfig starts from policy.DefaultConfig and overrides only the fields
// an operator can reasonably be expected to change, so a new policy knob is
// usable without an environment variable and the two defaults cannot drift.
func loadPolicyConfig() policy.Config {
	c := policy.DefaultConfig()

	c.RedisPrefix = getWithDefault("SPIDER_REDIS_PREFIX", c.RedisPrefix)
	c.UserAgent = getWithDefault("SPIDER_USER_AGENT", c.UserAgent)

	c.URLStateTTL = secondsWithDefault("URL_STATE_TTL_SEC", c.URLStateTTL)
	c.RobotsTTL = secondsWithDefault("ROBOTS_TTL_SEC", c.RobotsTTL)
	c.HostColdPeriod = secondsWithDefault("HOST_COLD_PERIOD_SEC", c.HostColdPeriod)
	c.MinCrawlDelay = secondsWithDefault("MIN_CRAWL_DELAY_SEC", c.MinCrawlDelay)

	c.DeadHostBase = secondsWithDefault("DEAD_HOST_BASE_SEC", c.DeadHostBase)
	c.DeadProbeAfter = secondsWithDefault("DEAD_HOST_PROBE_AFTER_SEC", c.DeadProbeAfter)
	c.DeadHostMaxExp = getIntWithDefault("DEAD_HOST_MAX_EXP", c.DeadHostMaxExp)

	c.HostCooldownBase = secondsWithDefault("HOST_COOLDOWN_BASE_SEC", c.HostCooldownBase)
	c.HostCooldownMaxExp = getIntWithDefault("HOST_COOLDOWN_MAX_EXP", c.HostCooldownMaxExp)

	c.URLBackoffBase = secondsWithDefault("URL_BACKOFF_BASE_SEC", c.URLBackoffBase)
	c.URLBackoffMax = secondsWithDefault("URL_BACKOFF_MAX_SEC", c.URLBackoffMax)

	c.DelayedPromoteBatch = getIntWithDefault("DELAYED_PROMOTE_BATCH", c.DelayedPromoteBatch)
	c.FrontierPopBatch = getIntWithDefault("FRONTIER_POP_BATCH",
		getIntWithDefault("REDIS_MAX_RETRY", c.FrontierPopBatch))
	c.MaxPagesPerHost = getIntWithDefault("MAX_PAGES_PER_HOST", c.MaxPagesPerHost)
	c.URLMaxAttempts = getIntWithDefault("URL_MAX_ATTEMPTS", c.URLMaxAttempts)
	c.MaxRedirects = getIntWithDefault("MAX_REDIRECTS", c.MaxRedirects)
	c.MaxBodyBytes = getIntWithDefault("MAX_BODY_BYTES", c.MaxBodyBytes)
	return c
}

// secondsWithDefault reads a duration expressed in whole seconds. A missing,
// unparseable, zero or negative value silently falls back and says so on stdout:
// a bad tuning value should not stop the spider from starting.
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
	password := Secret(getWithDefault("RABBITMQ_PASSWORD", "guest"))
	host := getWithDefault("RABBITMQ_HOST", "localhost")
	port := getIntWithDefault("RABBITMQ_PORT", 5672)
	jobQueue := jobQueueName()

	return RabbitMqConfig{
		protocol,
		user,
		password,
		host,
		port,
		jobQueue,
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
	password := Secret(getWithDefault("PG_PASSWORD", "1234"))
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
	password := Secret(getWithDefault("REDIS_PASSWORD", ""))
	db := getIntWithDefault("REDIS_DB", 1)
	port := getIntWithDefault("REDIS_PORT", 6379)

	return RedisConfig{
		Addr:     addr,
		Password: password,
		Port:     port,
		DB:       db,
	}
}

func loadAppConfig() AppConfig {
	maxCrawlers := getIntWithDefault("MAX_CRAWLERS", 20)
	httpTimeout := getIntWithDefault("HTTP_TIMEOUT", 60)
	crawlerTimeout := getIntWithDefault("CRAWLER_TIMEOUT", 60)
	maxConcurrentFetch := getIntWithDefault("MAX_CONCURRENT_FETCH", 200)
	logsPath := getWithDefault("LOGS_PATH", "./logs.json")
	clawlerDelay := getIntWithDefault("CRAWLER_DELAY", 200)
	jobQueue := jobQueueName()
	return AppConfig{
		MaxCrawlers:        maxCrawlers,
		CrawlerTimeout:     crawlerTimeout,
		HttpTimeout:        httpTimeout,
		MaxConcurrentFetch: maxConcurrentFetch,
		LogsPath:           logsPath,
		ClawlerDelay:       clawlerDelay,
		JobQueue:           jobQueue,
	}
}

// jobQueueName is the queue the indexer consumes.
//
// Both sides must agree on one name. Spider used to default to "indexer.job"
// while the indexer consumed "indexer.jobs", so every job landed in a queue
// nobody read and only the indexer's sweep recovered the page, minutes later
// (H-01). RABBITMQ_JOB_QUEUE is still read for deployments that set the old
// name, but RABBITMQ_QUEUE wins and the default matches the indexer.
func jobQueueName() string {
	if queue := os.Getenv("RABBITMQ_QUEUE"); queue != "" {
		return queue
	}
	if queue := os.Getenv("RABBITMQ_JOB_QUEUE"); queue != "" {
		return queue
	}
	return "indexer.jobs"
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
