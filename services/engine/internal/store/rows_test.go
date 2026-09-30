package store

// Deterministic coverage for the row-reading loop, with no database involved.
//
// The only way to see a truncated result set through `database/sql` is a failure
// that arrives *after* some rows have already been read, and provoking that
// against a real Postgres means killing a connection mid-query and hoping. So the
// driver is faked here instead: `faultyRows` hands back one good row and then
// fails, which is exactly the shape of a connection dropped partway through a
// large result set.
//
// This matters because of what it looked like. `Next` reports "no more rows" and
// "the connection died" the same way -- false -- and the difference is only in
// `Err`. The loop in `GetData` did not consult it, so a query cut short returned
// a short page and no error at all: the user saw a normal-looking result list
// with results missing from the end of it, and nothing anywhere recorded that the
// query had been cut off. For a search engine that is the worst available failure
// mode, because there is nothing to notice.

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

// A failure a real driver can produce mid-stream.
var errStreamTruncated = errors.New("connection reset by peer")

// ── a minimal database/sql driver that fails partway through a result set ────

type faultyDriver struct {
	rows int
	// failAfter is how many rows are delivered before the stream breaks. A
	// negative value means the stream never breaks.
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

// QueryerContext lets `sql.DB` call straight through without preparing.
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
	// Must match the column list `GetData` scans.
	return []string{"id", "url", "pr", "metadata", "word_count", "word_set"}
}

func (r *faultyRows) Close() error { return nil }

func (r *faultyRows) Next(dest []driver.Value) error {
	if r.failAfter >= 0 && r.delivered >= r.failAfter {
		// A non-EOF error is what makes `rows.Err()` report something.
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

// openFaultyStore builds a PsqlStore backed by the fake driver, and skips if the
// driver name has somehow been taken (which would mean two tests racing).
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

// ── the tests ───────────────────────────────────────────────────────────────

// TestGetDataReportsATruncatedResultSet is the regression test. Five rows exist
// and the stream dies after two: the answer has to be an error, because the
// alternative is a result list that is quietly missing three results.
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

// The failure has to arrive as an AppError, like every other store failure, so
// the handler renders "internal server error" rather than the driver's text.
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

// A stream that ends cleanly is not a failure, and treating it as one would break
// every search.
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

// A stream that breaks before yielding anything is a failure too, and used to be
// indistinguishable from a query that matched nothing -- the worst version of the
// bug, because an empty result page is exactly what a failed query looks like to
// a user.
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

// An empty result set is a legitimate answer, not a failure.
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

// A row whose metadata is not valid JSON is a scan-level failure and has to be
// reported rather than silently producing a page with empty metadata, which the
// results template would render as a bare URL with no title.
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
