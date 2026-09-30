package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"uuid"

	"github.com/Hassan-ach/boogle/services/spider/internal/config"
	"github.com/Hassan-ach/boogle/services/spider/internal/entity"
	"github.com/Hassan-ach/boogle/services/spider/internal/policy"
	"github.com/Hassan-ach/boogle/services/spider/internal/utils"
)

// Cache is the host-metadata half of the store's Redis usage.
//
// The frontier, the visited set and the delay bookkeeping used to be here too,
// and are now policy.State. They moved because they are crawl decisions, not
// storage: the old frontier script destroyed the entries it popped, and nothing
// in this file could have prevented that, because nothing in this file knew what
// a decision was.
type Cache interface {
	AddHostMetaData(ctx context.Context, h string, host *entity.Host) error
	GetHostMetaData(ctx context.Context, h string) (*entity.Host, bool, error)
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
	state  policy.State
	config *config.StoreConfig
	log    *slog.Logger
}

// NewStore builds a store over an already-open cache connection and the policy
// state layered on the same connection.
//
// Both are parameters because they are one thing. The policy manager owns the
// frontier and the visited set; if the store reached into its own cache for them
// there would be two frontiers, and the one the crawl read would not be the one
// the crawl wrote.
func NewStore(conf config.StoreConfig, log *utils.Logger, cache Cache, state policy.State) *Store {
	return &Store{
		db:     NewDbClient(conf.DB),
		cache:  cache,
		state:  state,
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
	// The page is marked visited, and only then are its links enqueued.
	//
	// Note what this does and does not prevent. Enqueue does not consult the
	// visited set -- the old AddUrls did, in a pipeline of SISMEMBERs, one extra
	// round trip over every link on every page, to avoid queueing something the
	// pop was going to discard anyway. So a page that links to itself does land
	// back in the frontier. It is not crawled again because the pop checks the
	// visited set before handing anything out, which is the same check, done once
	// per pop instead of once per link.
	err = s.state.MarkVisited(ctx, page.URL)
	if err != nil {
		s.log.Warn("add URL to visited set", "url", page.URL, "error", err)
		return pageID, err
	}
	err = s.state.Enqueue(ctx, page.Links...)
	if err != nil {
		s.log.Warn("add linked URLs to frontier", "url", page.URL, "error", err)
	}
	return pageID, nil
}

func (s *Store) persistHost(ctx context.Context, host *entity.Host) {
	err := s.cache.AddHostMetaData(ctx, host.Name, host)
	if err != nil {
		s.log.Warn("add host metadata to cache", "host", host.Name, "error", err)
	}
}

func (s *Store) GetHostMetaData(ctx context.Context, h string) (*entity.Host, bool, error) {
	return s.cache.GetHostMetaData(ctx, h)
}

// Init seeds the frontier with the start URLs, unless there is work already
// queued.
//
// A frontier length that cannot be read is an error, not an empty frontier. The
// old CountUrls swallowed the failure and returned 0, which read as "empty" and
// therefore as "seed it" -- so a Redis blip at startup re-enqueued every start URL
// on top of a frontier that was already full, and the URLs already crawled got
// their priority counted up again for no reason.
func (s *Store) Init(starters []string) error {
	ctx := context.Background()

	queued, err := s.state.FrontierLen(ctx)
	if err != nil {
		return fmt.Errorf("read frontier length: %w", err)
	}
	if queued > 0 {
		s.log.Info("frontier already has work; not seeding",
			"queued", queued, "starters", len(starters))
		return nil
	}

	if err := s.state.Enqueue(ctx, starters...); err != nil {
		return fmt.Errorf("seed frontier: %w", err)
	}
	s.log.Info("frontier seeded", "starters", len(starters))
	return nil
}

func (s *Store) Close() {
	s.db.Close()
	s.cache.Close()
}
