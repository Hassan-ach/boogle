package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/url"
	"strconv"
	"time"

	"github.com/Hassan-ach/boogle/services/engine/internal/apperror"
	"github.com/Hassan-ach/boogle/services/engine/internal/config/store"
	"github.com/Hassan-ach/boogle/services/engine/internal/model"
	"github.com/Hassan-ach/boogle/services/engine/internal/util"

	"github.com/google/uuid"
	"github.com/lib/pq"
)

type Store interface {
	// GetData returns the top CandidateLimit matches for the query. Pagination
	// and final ordering are the ranker's and handler's job: the SQL pass cannot
	// see the TF-IDF/PageRank blend.
	GetData(c context.Context, words []string) (*Data, error)
	// GetTotalResults counts every page matching any query word.
	GetTotalResults(c context.Context, query []string) (int, error)
	PageSize() int
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
	// Built with url.URL so a password containing '@', ':' or '/' is escaped
	// instead of changing the connection target.
	conn, err := sql.Open("postgres", connectionString(conf))
	if err != nil {
		panic(fmt.Sprintf("Failed to open database connection: %v", err))
	}

	// A black-holed host must fail boot quickly instead of hanging forever.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err = conn.PingContext(ctx); err != nil {
		log.Fatalln("Failed to connect to database:", err)
	}

	conn.SetMaxOpenConns(conf.DB.MaxOpenConns)
	conn.SetMaxIdleConns(conf.DB.MaxIdleConns)

	return PsqlStore{
		conn: conn,
		conf: conf,
	}
}

// GetData returns the candidate window for the given query words.
//
// Ranking happens in SQL rather than in Go: the CTE groups by page, counts
// distinct matching words and aggregates each page's tf/idf pairs, then orders by
// that count. The result is capped at CandidateLimit, and the Go side re-ranks
// that whole window with cosine similarity before the handler slices it into
// pages. Ordering here is the coarse pass (coverage, then PageRank); the engine's
// blend is the fine one.
func (s PsqlStore) GetData(c context.Context, words []string) (*Data, error) {
	sql := `
		WITH ranked AS (
		    SELECT
		        p.id,
		        u.url,
		        COALESCE(pr.score, 0) AS pr,
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
		    LEFT JOIN page_rank pr  ON p.url_id = pr.url_id
		    INNER JOIN urls u       ON p.url_id = u.id
		    WHERE w.word = ANY($1)
		    GROUP BY p.id, pr.score, u.url, p.metadata
		)
		SELECT *
		FROM ranked
		ORDER BY word_count DESC,
		         pr DESC,
		         id ASC
		LIMIT $2`

	candidateLimit := s.conf.CandidateLimit
	if candidateLimit < 1 {
		// A hand-built config must not turn every search into LIMIT 0.
		candidateLimit = 100
	}

	rows, err := s.conn.QueryContext(
		c,
		sql,
		pq.Array(words),
		candidateLimit,
	)
	if err != nil {
		return nil, apperror.Internal(fmt.Errorf("failed to execute query: %w", err))
	}
	defer func() { _ = rows.Close() }()

	wordIdf := make(map[string]float64, len(words))
	pgs := make([]*model.Page, 0, s.conf.CandidateLimit)

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

// GetTotalResults counts every page matching any query word, not just the window
// GetData returns. The handler divides this by PageSize so the UI reports the
// number of pages, not one page per match.
func (s PsqlStore) GetTotalResults(c context.Context, query []string) (int, error) {
	sql := `
		SELECT COUNT(DISTINCT p.id)
		FROM words w
		JOIN page_word pw ON w.id = pw.word_id
		JOIN pages p      ON pw.page_id = p.id
		WHERE w.word = ANY($1)`

	if len(query) == 0 {
		return 0, nil
	}

	var total int
	err := s.conn.QueryRowContext(c, sql, pq.Array(query)).Scan(&total)
	if err != nil {
		return 0, apperror.Internal(fmt.Errorf("failed to count matching pages: %w", err))
	}
	return total, nil
}

// PageSize is the number of results rendered per page.
func (s PsqlStore) PageSize() int {
	return s.conf.PageSize
}

// connectionString builds a libpq URL without manual escaping.
func connectionString(conf store.StoreConfig) string {
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(conf.DB.User, conf.DB.Password),
		Host:   net.JoinHostPort(conf.DB.Host, strconv.Itoa(conf.DB.Port)),
		Path:   conf.DB.DBName,
	}
	q := u.Query()
	sslMode := conf.DB.SSLMode
	if sslMode == "" {
		// lib/pq treats an empty sslmode as "require"; a hand-built config or an
		// old env file must not silently start negotiating TLS.
		sslMode = "disable"
	}
	q.Set("sslmode", sslMode)
	q.Set("connect_timeout", "5")
	// A search that runs away must not hold a pool connection forever.
	q.Set("statement_timeout", "30000")
	u.RawQuery = q.Encode()
	return u.String()
}
