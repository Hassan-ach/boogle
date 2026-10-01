package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"uuid"

	_ "github.com/lib/pq"

	"github.com/Hassan-ach/boogle/services/spider/internal/config"
	"github.com/Hassan-ach/boogle/services/spider/internal/entity"
)

type SQLClient struct {
	conn *sql.DB
}

func NewDbClient(conf config.PSQLConfig) *SQLClient {
	psqlconn := fmt.Sprintf(
		"host=%s port=%d user=%s password=%s dbname=%s sslmode=disable",
		conf.Host,
		conf.Port,
		conf.User,
		conf.Password,
		conf.DBname,
	)

	db, err := sql.Open("postgres", psqlconn)
	if err != nil {
		log.Fatalf(
			"Failed to connect to postgres ERROR: %v",
			err,
		)
	}

	err = db.Ping()
	if err != nil {
		log.Fatalf("Failed to ping postgres ERROR: %v", err)
	}

	db.SetConnMaxLifetime(conf.MaxConnLifetime)
	db.SetMaxOpenConns(conf.MaxOpenConns)
	db.SetMaxIdleConns(conf.MaxIdleConns)

	fmt.Println("Data Base Connectd")

	return &SQLClient{
		conn: db,
	}
}

func (c *SQLClient) Close() {
	_ = c.conn.Close()
}

func (c *SQLClient) WithTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := c.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}

	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}

	if err = tx.Commit(); err != nil {
		return err
	}

	return nil
}

func (c *SQLClient) InsertPage(ctx context.Context, tx *sql.Tx, page *entity.Page) (uuid.UUID, error) {
	var url_id string
	err := tx.QueryRowContext(ctx,
		`WITH ins AS (
		    INSERT INTO urls(url)
		    VALUES ($1)
		    ON CONFLICT (url) DO NOTHING
		    RETURNING id
		)
		SELECT id FROM ins
		UNION ALL
		SELECT id FROM urls WHERE url = $1
		LIMIT 1;`,
		page.URL).Scan(&url_id)
	if err != nil {
		return uuid.Nil(), fmt.Errorf("upsert url: %w", err)
	}

	metadata, err := json.Marshal(page.MetaData)
	if err != nil {
		return uuid.Nil(), fmt.Errorf("marshal metadata: %w", err)
	}

	var id uuid.UUID

	err = tx.QueryRowContext(ctx,
		`INSERT INTO pages(url_id, html, metadata) VALUES ($1,$2,$3)
		ON CONFLICT (url_id) DO UPDATE SET metadata = EXCLUDED.metadata
		RETURNING id;`,
		url_id,
		page.HTML,
		metadata,
	).Scan(&id)
	if err != nil {
		return uuid.Nil(), fmt.Errorf("insert page : %w", err)
	}

	return id, nil
}

func (c *SQLClient) InsertGraphEdges(
	ctx context.Context,
	tx *sql.Tx,
	from_url_id string,
	to_url_ids []string,
) error {
	if len(to_url_ids) == 0 {
		return nil
	}

	var (
		placeholders []string
		args         []any
	)

	for _, to_url_id := range to_url_ids {
		placeholders = append(placeholders, fmt.Sprintf("($%d, $%d)", len(args)+1, len(args)+2))
		args = append(args, from_url_id, to_url_id)
	}

	query := `INSERT INTO graph_edges (from_url, to_url) VALUES ` +
		strings.Join(placeholders, ", ") +
		` ON CONFLICT DO NOTHING`

	_, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("batch insert graph_edges: %w", err)
	}

	return nil
}

func (c *SQLClient) InsertURLs(ctx context.Context, tx *sql.Tx, urls []string) ([]string, error) {
	if len(urls) == 0 {
		return make([]string, 0), nil
	}

	var values []string
	args := make([]any, 0, len(urls))

	for i, u := range urls {
		values = append(values, fmt.Sprintf("($%d)", i+1))
		args = append(args, u)
	}

	query := fmt.Sprintf(`
		WITH input(url) AS (
			SELECT DISTINCT url FROM (VALUES %s) AS v(url)
		),
		ins AS (
		    INSERT INTO urls (url)
		    SELECT url FROM input
			ON CONFLICT (url) DO NOTHING
		    RETURNING id, url
		)
		SELECT u.id
		FROM input i
		JOIN urls u ON u.url = i.url
		`, strings.Join(values, ","))

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("batch upsert query failed: %w", err)
	}
	defer func() { _ = rows.Close() }()

	m := make([]string, 0, len(urls))

	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan failed: %w", err)
		}
		m = append(m, id)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return m, nil
}
