package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"uuid"

	"github.com/Hassan-ach/boogle/services/spider/internal/config"
	"github.com/Hassan-ach/boogle/services/spider/internal/entity"
	"github.com/Hassan-ach/boogle/services/spider/internal/utils"
)

type Cache interface {
	AddHostMetaData(ctx context.Context, h string, host *entity.Host) error
	GetHostMetaData(ctx context.Context, h string) (*entity.Host, bool, error)
	GetUrl(ctx context.Context) (string, bool, error)
	AddUrls(ctx context.Context, urls []string) error
	MarkVisited(ctx context.Context, u string) error
	AddToWaitedHost(ctx context.Context, h string, delay int) error
	CountUrls(ctx context.Context) int64
	Close()
}
type DB interface {
	InsertPage(ctx context.Context, tx *sql.Tx, page *entity.Page) (uuid.UUID, error)
	InsertGraphEdges(ctx context.Context, tx *sql.Tx, from_url_id string, to_url_ids []string) error
	InsertURLs(ctx context.Context, tx *sql.Tx, urls []string) ([]string, error)
	WithTx(ctx context.Context, fn func(tx *sql.Tx) error) error
	Close()
}

type Store struct {
	db     DB
	cache  Cache
	config *config.StoreConfig
	log    *slog.Logger
}

func NewStore(conf config.StoreConfig, log *utils.Logger) *Store {
	db := NewDbClient(conf.DB)
	rd := NewRedisClient(conf.Cache)

	return &Store{
		db:     db,
		cache:  rd,
		config: &conf,
		log:    log.With("component", "store"),
	}
}

func (s *Store) GetCache() Cache {
	return s.cache
}

func (s *Store) Persist(ctx context.Context, page *entity.Page, host *entity.Host) uuid.UUID {
	s.log.Info("Persisting page and host metadata", "url", page.URL, "host", host.Name)
	s.persistHost(ctx, host)
	pageID, err := s.persistPage(ctx, page)
	if err != nil {
		s.log.Error("persist page data", "url", page.URL, "error", err)
		return pageID
	}
	return pageID
}

func (s *Store) persistPage(ctx context.Context, page *entity.Page) (pageID uuid.UUID, err error) {
	s.log.Info("Persisting page data", "url", page.URL)

	if err = s.db.WithTx(ctx, func(tx *sql.Tx) error {
		if pageID, err = s.db.InsertPage(ctx, tx, page); err != nil {
			s.log.Error("insert page into database", "url", page.URL, "err", err)
			return fmt.Errorf("insert page: %w", err)
		}
		return nil
	}); err != nil {
		s.log.Warn("failed to persist page", "url", page.URL, "err", err)
		return pageID, fmt.Errorf("persist page in database: %w", err)
	}

	if err = s.db.WithTx(ctx, func(tx *sql.Tx) error {
		ids, err := s.db.InsertURLs(
			ctx,
			tx,
			append([]string{page.URL}, page.Links...),
		)
		if err != nil {
			return fmt.Errorf("insert URLs into database: %w", err)
		}
		err = s.db.InsertGraphEdges(ctx, tx, ids[0], ids[1:])
		if err != nil {
			return fmt.Errorf("insert graph edges into database: %w", err)
		}
		return nil
	}); err != nil {
		s.log.Warn("", "url", page.URL, "err", err)
		return pageID, err
	}
	err = s.cache.MarkVisited(ctx, page.URL)
	if err != nil {
		s.log.Warn("add URL to visited set", "url", page.URL, "error", err)
		return pageID, err
	}
	err = s.cache.AddUrls(ctx, page.Links)
	if err != nil {
		s.log.Warn("add linked URLs to cache", "url", page.URL, "error", err)
	}
	return pageID, nil
}

func (s *Store) persistHost(ctx context.Context, host *entity.Host) {
	err := s.cache.AddToWaitedHost(ctx, host.Name, host.Delay)
	if err != nil {
		s.log.Warn("add host to waited set", "host", host.Name, "error", err)
	}
	err = s.cache.AddHostMetaData(ctx, host.Name, host)
	if err != nil {
		s.log.Warn("add host metadata to cache", "host", host.Name, "error", err)
	}
}

func (s *Store) GetNextUrl(ctx context.Context) (string, bool, error) {
	// i need to handle err and fetching from db
	return s.cache.GetUrl(ctx)
}

func (s *Store) GetHostMetaData(ctx context.Context, h string) (*entity.Host, bool, error) {
	return s.cache.GetHostMetaData(ctx, h)
}

func (s *Store) Init(starters []string) error {
	if s.cache.CountUrls(context.Background()) > 0 {
		return nil
	}
	err := s.cache.AddUrls(context.Background(), starters)
	if err != nil {
		return err
	}
	return nil
}

func (s *Store) Close() {
	s.db.Close()
	s.cache.Close()
}
