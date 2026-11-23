package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"uuid"

	"github.com/Hassan-ach/boogle/services/spider/internal/config"
	"github.com/Hassan-ach/boogle/services/spider/internal/entity"
	"github.com/Hassan-ach/boogle/services/spider/internal/state"
	"github.com/Hassan-ach/boogle/services/spider/internal/utils"
)

// Cache is the Redis connection the store holds open. Host state and the
// frontier live under state.RedisState's keys, so all this has to do is share
// the pool and close it.
type Cache interface {
	Close()
}
type DB interface {
	InsertPage(ctx context.Context, tx *sql.Tx, page *entity.Page) (uuid.UUID, string, error)
	InsertGraphEdges(ctx context.Context, tx *sql.Tx, from_url_id string, to_url_ids []string) error
	InsertURLs(ctx context.Context, tx *sql.Tx, urls []string) (map[string]string, error)
	WithTx(ctx context.Context, fn func(tx *sql.Tx) error) error
	Close()
}

type Store struct {
	db     DB
	cache  Cache
	st     state.State
	config *config.StoreConfig
	log    *slog.Logger
}

func NewStore(conf config.StoreConfig, log *utils.Logger, cache Cache, st state.State) *Store {
	return &Store{
		db:     NewDbClient(conf.DB),
		cache:  cache,
		st:     st,
		config: &conf,
		log:    log.With("component", "store"),
	}
}

// Persist writes a page and its edges. The error is part of the contract: a
// non-nil page id is not proof of a commit, because it is assigned before the
// transaction commits.
func (s *Store) Persist(ctx context.Context, page *entity.Page) (uuid.UUID, error) {
	s.log.Info("Persisting page data", "url", page.URL)
	pageID, err := s.persistPage(ctx, page)
	if err != nil {
		s.log.Error("persist page data", "url", page.URL, "error", err)
		return pageID, err
	}
	return pageID, nil
}

func (s *Store) persistPage(ctx context.Context, page *entity.Page) (pageID uuid.UUID, err error) {
	s.log.Info("Persisting page data", "url", page.URL)

	var pageURLID string

	// One transaction for the page, links and edges: a page without its graph
	// edges is incomplete for PageRank.
	if err = s.db.WithTx(ctx, func(tx *sql.Tx) error {
		pageID, pageURLID, err = s.db.InsertPage(ctx, tx, page)
		if err != nil {
			s.log.Error("insert page into database", "url", page.URL, "err", err)
			return fmt.Errorf("insert page: %w", err)
		}

		linkIDs, err := s.db.InsertURLs(ctx, tx, page.Links)
		if err != nil {
			return fmt.Errorf("insert URLs into database: %w", err)
		}

		toURLIDs := make([]string, 0, len(page.Links))
		for _, link := range page.Links {
			if link == page.URL {
				// A self-link is not an edge PageRank should see.
				continue
			}
			id, ok := linkIDs[link]
			if !ok {
				// A missing id would leave the link graph incomplete, so fail here.
				return fmt.Errorf("no url id for admitted link %q", link)
			}
			toURLIDs = append(toURLIDs, id)
		}

		if err := s.db.InsertGraphEdges(ctx, tx, pageURLID, toURLIDs); err != nil {
			return fmt.Errorf("insert graph edges into database: %w", err)
		}
		return nil
	}); err != nil {
		s.log.Warn("failed to persist page and its edges", "url", page.URL, "err", err)
		return pageID, err
	}
	return pageID, nil
}

func (s *Store) Init(starters []string) error {
	ctx := context.Background()

	queued, err := s.st.FrontierLen(ctx)
	if err != nil {
		return fmt.Errorf("read frontier length: %w", err)
	}
	if queued > 0 {
		s.log.Info("frontier already has work; not seeding",
			"queued", queued, "starters", len(starters))
		return nil
	}

	if err := s.st.Enqueue(ctx, starters...); err != nil {
		return fmt.Errorf("seed frontier: %w", err)
	}
	s.log.Info("frontier seeded", "starters", len(starters))
	return nil
}

func (s *Store) Close() {
	s.db.Close()
	s.cache.Close()
}
