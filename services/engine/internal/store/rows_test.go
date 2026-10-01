package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Hassan-ach/boogle/services/engine/internal/config/store"
	"github.com/google/uuid"
)

var errStreamTruncated = errors.New("connection reset by peer")

type faultyDriver struct {
	rows      int
	failAfter int
}

type faultyConn struct {
	rows      int
	failAfter int
}

func (d *faultyDriver) Open(string) (driver.Conn, error) {
	return &faultyConn{rows: d.rows, failAfter: d.failAfter}, nil
}

func (c *faultyConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("Prepare is not implemented by the test driver")
}
func (c *faultyConn) Close() error              { return nil }
func (c *faultyConn) Begin() (driver.Tx, error) { return nil, errors.New("not implemented") }

func (c *faultyConn) QueryContext(
	_ context.Context, _ string, _ []driver.NamedValue,
) (driver.Rows, error) {
	return &faultyRows{remaining: c.rows, failAfter: c.failAfter}, nil
}

type faultyRows struct {
	remaining int
	failAfter int
	delivered int
}

func (r *faultyRows) Columns() []string {
	return []string{"id", "url", "pr", "metadata", "word_count", "word_set"}
}

func (r *faultyRows) Close() error { return nil }

func (r *faultyRows) Next(dest []driver.Value) error {
	if r.failAfter >= 0 && r.delivered >= r.failAfter {
		return errStreamTruncated
	}
	if r.remaining == 0 {
		return io.EOF
	}
	r.remaining--
	r.delivered++

	pageID := uuid.New()
	meta, err := json.Marshal(map[string]any{"url": "https://faulty.test/", "title": "t"})
	if err != nil {
		return err
	}
	words, err := json.Marshal([]map[string]any{{"word": "alpha", "idf": 1.5, "tf": 3}})
	if err != nil {
		return err
	}

	dest[0] = pageID.String()
	dest[1] = "https://faulty.test/"
	dest[2] = 0.25
	dest[3] = meta
	dest[4] = int64(1)
	dest[5] = words
	return nil
}

var driverCounter atomic.Int64

func openFaultyStore(t *testing.T, rowCount, failAfter int) PsqlStore {
	t.Helper()
	name := "boogle-faulty-" + strings.ReplaceAll(t.Name(), "/", "-") +
		itoa(driverCounter.Add(1))
	sql.Register(name, &faultyDriver{rows: rowCount, failAfter: failAfter})
	conn, err := sql.Open(name, "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return PsqlStore{conn: conn, conf: store.StoreConfig{PageSize: 20}}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var out []byte
	for n > 0 {
		out = append([]byte{byte('0' + n%10)}, out...)
		n /= 10
	}
	return string(out)
}

func TestGetDataReportsATruncatedResultSet(t *testing.T) {
	s := openFaultyStore(t, 5, 2)

	data, err := s.GetData(context.Background(), []string{"alpha"}, 0)
	if err == nil {
		t.Fatalf("GetData returned %d pages and no error after the stream broke",
			len(data.Pages))
	}
	if !errors.Is(err, errStreamTruncated) {
		t.Errorf("error = %v, want it to wrap %v", err, errStreamTruncated)
	}
}

func TestGetDataRendersATruncatedResultSetAsAnAppError(t *testing.T) {
	s := openFaultyStore(t, 5, 2)

	_, err := s.GetData(context.Background(), []string{"alpha"}, 0)
	if err == nil {
		t.Fatal("no error")
	}
	if got := err.Error(); got != "internal server error" {
		t.Errorf("rendered error = %q, want the generic message", got)
	}
	if !strings.Contains(err.Error(), "failed while reading results") &&
		!errors.Is(err, errStreamTruncated) {
		t.Errorf("error = %v, want it to name the read failure", err)
	}
}

func TestGetDataAcceptsAStreamThatEndsCleanly(t *testing.T) {
	s := openFaultyStore(t, 3, -1)

	data, err := s.GetData(context.Background(), []string{"alpha"}, 0)
	if err != nil {
		t.Fatalf("GetData: %v", err)
	}
	if len(data.Pages) != 3 {
		t.Errorf("got %d pages, want 3", len(data.Pages))
	}
	for _, p := range data.Pages {
		if p.Words["alpha"] != 3 {
			t.Errorf("tf for alpha = %d, want 3", p.Words["alpha"])
		}
		if p.MetaData.Title != "t" {
			t.Errorf("metadata did not decode: %+v", p.MetaData)
		}
		if p.PRScore != 0.25 {
			t.Errorf("PRScore = %v, want 0.25", p.PRScore)
		}
	}
}

func TestGetDataReportsAStreamThatBreaksBeforeAnyRow(t *testing.T) {
	s := openFaultyStore(t, 5, 0)

	data, err := s.GetData(context.Background(), []string{"alpha"}, 0)
	if err == nil {
		t.Fatalf("GetData reported %d pages and no error", len(data.Pages))
	}
	if !errors.Is(err, errStreamTruncated) {
		t.Errorf("error = %v, want it to wrap the truncation", err)
	}
}

func TestGetDataAcceptsAnEmptyResultSet(t *testing.T) {
	s := openFaultyStore(t, 0, -1)

	data, err := s.GetData(context.Background(), []string{"alpha"}, 0)
	if err != nil {
		t.Fatalf("GetData: %v", err)
	}
	if len(data.Pages) != 0 {
		t.Errorf("got %d pages, want none", len(data.Pages))
	}
}

func TestGetDataReportsARowWithUnreadableMetadata(t *testing.T) {
	sql.Register("boogle-faulty-badmeta", &badMetaDriver{})
	conn, err := sql.Open("boogle-faulty-badmeta", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	s := PsqlStore{conn: conn, conf: store.StoreConfig{PageSize: 20}}

	_, err = s.GetData(context.Background(), []string{"alpha"}, 0)
	if err == nil {
		t.Fatal("a row with unreadable metadata was accepted")
	}
	if got := err.Error(); got != "internal server error" {
		t.Errorf("rendered error = %q, want the generic message", got)
	}
}

type badMetaDriver struct{}

func (badMetaDriver) Open(string) (driver.Conn, error) { return &badMetaConn{}, nil }

type badMetaConn struct{ faultyConn }

func (c *badMetaConn) QueryContext(
	_ context.Context, _ string, _ []driver.NamedValue,
) (driver.Rows, error) {
	return &badMetaRows{}, nil
}

type badMetaRows struct{ delivered bool }

func (r *badMetaRows) Columns() []string {
	return (&faultyRows{}).Columns()
}
func (r *badMetaRows) Close() error { return nil }

func (r *badMetaRows) Next(dest []driver.Value) error {
	if r.delivered {
		return io.EOF
	}
	r.delivered = true
	dest[0] = uuid.New().String()
	dest[1] = "https://badmeta.test/"
	dest[2] = 0.1
	dest[3] = []byte("{not json")
	dest[4] = int64(1)
	dest[5] = []byte(`[{"word":"alpha","idf":1.0,"tf":1}]`)
	return nil
}
