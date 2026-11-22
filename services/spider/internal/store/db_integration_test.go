//go:build integration

package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/Hassan-ach/boogle/services/spider/internal/config"
	"github.com/Hassan-ach/boogle/services/spider/internal/entity"
)

// testClient opens the integration database, skipping when no server is
// configured. NewDbClient exits the process on a failed ping, so reachability is
// checked with a throwaway handle first.
func testClient(t *testing.T) *SQLClient {
	t.Helper()

	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}

	probe, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Skipf("cannot open %s: %v", dsn, err)
	}
	if err := probe.Ping(); err != nil {
		_ = probe.Close()
		t.Skipf("no database at %s: %v", dsn, err)
	}
	_ = probe.Close()

	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("TEST_DATABASE_URL is not a URL: %v", err)
	}
	port, _ := strconv.Atoi(parsed.Port())
	password, _ := parsed.User.Password()

	client := NewDbClient(config.PSQLConfig{
		Host:            parsed.Hostname(),
		Port:            port,
		User:            parsed.User.Username(),
		Password:        config.Secret(password),
		DBname:          strings.TrimPrefix(parsed.Path, "/"),
		MaxOpenConns:    4,
		MaxIdleConns:    4,
		MaxConnLifetime: time.Minute,
	})
	t.Cleanup(client.Close)
	return client
}

func TestInsertURLsReturnsIDsForNewAndExistingURLs(t *testing.T) {
	client := testClient(t)
	ctx := context.Background()
	tag := fmt.Sprintf("%d", time.Now().UnixNano())

	existing := fmt.Sprintf("https://it-existing-%s.test/", tag)
	newOne := fmt.Sprintf("https://it-new-one-%s.test/", tag)
	newTwo := fmt.Sprintf("https://it-new-two-%s.test/", tag)
	t.Cleanup(func() {
		_, _ = client.conn.Exec("DELETE FROM urls WHERE url LIKE $1", "https://it-%"+tag+"%")
	})

	var existingID string
	if err := client.conn.QueryRow(
		"INSERT INTO urls (url) VALUES ($1) RETURNING id", existing,
	).Scan(&existingID); err != nil {
		t.Fatalf("seed existing url: %v", err)
	}

	page := &entity.Page{
		URL:   fmt.Sprintf("https://it-page-%s.test/", tag),
		Links: []string{existing, newOne, newTwo},
	}

	var pageURLID string
	err := client.WithTx(ctx, func(tx *sql.Tx) error {
		var pageID uuid.UUID
		var err error
		pageID, pageURLID, err = client.InsertPage(ctx, tx, page)
		if err != nil {
			return err
		}
		if pageID == uuid.Nil() {
			return fmt.Errorf("InsertPage returned a nil id")
		}

		ids, err := client.InsertURLs(ctx, tx, page.Links)
		if err != nil {
			return err
		}
		if len(ids) != 3 {
			return fmt.Errorf("InsertURLs returned %d ids, want 3: %v", len(ids), ids)
		}
		if ids[existing] != existingID {
			return fmt.Errorf("existing url mapped to %q, want %q", ids[existing], existingID)
		}
		if ids[newOne] == "" || ids[newTwo] == "" || ids[newOne] == ids[newTwo] {
			return fmt.Errorf("new urls got bad ids: %v", ids)
		}

		return client.InsertGraphEdges(ctx, tx, pageURLID, []string{ids[existing], ids[newOne], ids[newTwo]})
	})
	if err != nil {
		t.Fatalf("persist page and edges: %v", err)
	}

	var edges int
	if err := client.conn.QueryRow(
		"SELECT count(*) FROM graph_edges WHERE from_url = $1", pageURLID,
	).Scan(&edges); err != nil {
		t.Fatalf("count edges: %v", err)
	}
	if edges != 3 {
		t.Errorf("stored %d edges, want 3", edges)
	}
}

func TestReciprocalEdgesAreBothStored(t *testing.T) {
	client := testClient(t)
	ctx := context.Background()
	tag := fmt.Sprintf("%d", time.Now().UnixNano())

	a := fmt.Sprintf("https://it-recip-a-%s.test/", tag)
	b := fmt.Sprintf("https://it-recip-b-%s.test/", tag)

	var aID, bID string
	if err := client.conn.QueryRow("INSERT INTO urls (url) VALUES ($1) RETURNING id", a).Scan(&aID); err != nil {
		t.Fatalf("seed a: %v", err)
	}
	if err := client.conn.QueryRow("INSERT INTO urls (url) VALUES ($1) RETURNING id", b).Scan(&bID); err != nil {
		t.Fatalf("seed b: %v", err)
	}
	t.Cleanup(func() {
		_, _ = client.conn.Exec("DELETE FROM urls WHERE url LIKE $1", "https://it-recip-%"+tag+"%")
	})

	err := client.WithTx(ctx, func(tx *sql.Tx) error {
		if err := client.InsertGraphEdges(ctx, tx, aID, []string{bID}); err != nil {
			return err
		}
		return client.InsertGraphEdges(ctx, tx, bID, []string{aID})
	})
	if err != nil {
		t.Fatalf("insert reciprocal edges: %v", err)
	}

	var count int
	if err := client.conn.QueryRow(
		"SELECT count(*) FROM graph_edges WHERE (from_url = $1 AND to_url = $2) OR (from_url = $2 AND to_url = $1)",
		aID, bID,
	).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Errorf("stored %d of 2 reciprocal edges; the reverse unique index is back", count)
	}
}
