package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"

	"github.com/Hassan-ach/boogle/services/engine/internal/apperror"
	"github.com/Hassan-ach/boogle/services/engine/internal/config/store"
	"github.com/Hassan-ach/boogle/services/engine/internal/model"
	"github.com/Hassan-ach/boogle/services/engine/internal/util"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

type Store interface {
	GetData(c context.Context, words []string, pageNum int) (*Data, error)
	GetTotalPages(c context.Context, query []string) (int, error)
}

// Data is one query's worth of loaded state. The mappers come from the same load
// as Pages, which is what lets the ranker index into a dense matrix and why it
// panics on a missing entry rather than skipping.
type Data struct {
	Pages      []*model.Page
	Idf        map[string]float64
	WordMapper util.Mapper[string]
	PageMapper util.Mapper[uuid.UUID]
}
type PsqlStore struct {
	conn *sql.DB
	conf store.StoreConfig
}

// NewStore opens the pool and pings. Both failures are fatal: the engine serves
// nothing without Postgres, and failing at boot surfaces the misconfiguration
// instead of returning empty result pages to every query.
func NewStore(conf store.StoreConfig) PsqlStore {
	urls := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=disable",
		conf.DB.Host, conf.DB.Port, conf.DB.User, conf.DB.Password, conf.DB.DBName)

	conn, err := sql.Open("postgres", urls)
	if err != nil {
		panic(fmt.Sprintf("Failed to open database connection: %v", err))
	}

	err = conn.Ping()
	if err != nil {
		log.Fatalln("Failed to connect to database:", err)
	}

	conn.SetMaxOpenConns(conf.DB.MaxOpenConns)
	conn.SetMaxIdleConns(conf.DB.MaxIdleConns)

	return PsqlStore{
		conn: conn,
		conf: conf,
	}
}

// GetData returns one page of results for the given query words.
//
// Ranking happens in SQL rather than in Go: the CTE groups by page, counts
// distinct matching words and aggregates each page's tf/idf pairs, then orders by
// that count. So the candidate set is narrowed to PageSize rows in the database,
// and the Go side only re-ranks that window with cosine similarity. Ordering
// here is the coarse pass (coverage, then PageRank); the engine's blend is the
// fine one.
//
// pageNum is zero-based, so callers pass page-1 from the query string.
func (s PsqlStore) GetData(c context.Context, words []string, pageNum int) (*Data, error) {
	sql := `
		WITH ranked AS (
		    SELECT
		        p.id,
		        u.url,
		        pr.score AS pr,
		        p.metadata,
		        COUNT(DISTINCT w.id) AS word_count,
		        COALESCE(
		            json_agg(
		                json_build_object(
		                    'word', w.word,
		                    'idf',  w.idf,
							'tf',	pw.tf
		                )
		            ),
		            '[]'
		        ) AS word_set
		    FROM words w
		    INNER JOIN page_word pw ON w.id = pw.word_id
		    INNER JOIN pages p      ON pw.page_id = p.id
		    INNER JOIN page_rank pr ON p.url_id = pr.url_id
		    INNER JOIN urls u       ON p.url_id = u.id
		    WHERE w.word = ANY($1)
		    GROUP BY p.id, pr.score, u.url, p.metadata
		)
		SELECT *
		FROM ranked
		ORDER BY word_count DESC,
		         pr DESC,
		         id ASC
		LIMIT $2 OFFSET $3`

	rows, err := s.conn.QueryContext(
		c,
		sql,
		pq.Array(words),
		s.conf.PageSize,
		pageNum*s.conf.PageSize,
	)
	if err != nil {
		return nil, apperror.Internal(fmt.Errorf("failed to execute query: %w", err))
	}
	defer func() { _ = rows.Close() }()

	wordIdf := make(map[string]float64, len(words))
	pgs := make([]*model.Page, 0, s.conf.PageSize)

	for rows.Next() {
		var (
			id         uuid.UUID
			url        string
			prScore    float64
			metadata   []byte
			word_count int
			word_set   []byte

			meta    model.MetaData
			wordSet []model.Word
		)

		err := rows.Scan(&id, &url, &prScore, &metadata, &word_count, &word_set)
		if err != nil {
			return nil, apperror.Internal(fmt.Errorf("failed to scan data: %w", err))
		}

		err = json.Unmarshal(metadata, &meta)
		if err != nil {
			return nil, apperror.Internal(
				fmt.Errorf("failed to unmarshal metadata for url %s: %w", url, err),
			)
		}

		err = json.Unmarshal(word_set, &wordSet)
		if err != nil {
			return nil, apperror.Internal(
				fmt.Errorf("failed to unmarshal word set for url %s: %w", url, err),
			)
		}
		page := &model.Page{
			ID:       id,
			URL:      url,
			PRScore:  prScore,
			Words:    make(map[string]int, len(wordSet)),
			MetaData: meta,
		}

		for _, w := range wordSet {
			wordIdf[w.Word] = w.Idf
			page.Words[w.Word] = w.Tf
		}

		pgs = append(pgs, page)
	}

	if err := rows.Err(); err != nil {
		return nil, apperror.Internal(fmt.Errorf("failed while reading results: %w", err))
	}

	pageMapper := util.NewPageMapper()

	for _, page := range pgs {
		pageMapper.MapValue(page.ID)
	}
	wordMapper := util.NewWordMapper()
	for _, w := range words {
		wordMapper.MapValue(w)
	}

	return &Data{
		Pages:      pgs,
		Idf:        wordIdf,
		WordMapper: wordMapper,
		PageMapper: pageMapper,
	}, nil
}

// GetTotalPages counts every page matching any query word, not just the window
// GetData returns. It therefore counts pages the SQL ranking would never surface,
// so the page count is an upper bound on what a user can page through.
func (s PsqlStore) GetTotalPages(c context.Context, query []string) (int, error) {
	sql := `
		SELECT COUNT(DISTINCT p.id)
		FROM words w
		JOIN page_word pw ON w.id = pw.word_id
		JOIN pages p      ON pw.page_id = p.id
		WHERE w.word = ANY($1)`

	var total int
	err := s.conn.QueryRowContext(c, sql, pq.Array(query)).Scan(&total)
	if err != nil {
		return 0, apperror.Internal(fmt.Errorf("failed to get total pages: %w", err))
	}
	return total, nil
}
