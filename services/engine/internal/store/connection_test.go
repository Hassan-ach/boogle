package store

import (
	"net/url"
	"testing"

	storeconfig "github.com/Hassan-ach/boogle/services/engine/internal/config/store"
)

func TestConnectionStringEscapesCredentialsAndKeepsEveryField(t *testing.T) {
	conf := storeconfig.StoreConfig{
		DB: storeconfig.PsqlConfig{
			Host:     "db",
			Port:     5433,
			User:     "user@example.com",
			Password: "p@ss:w/rd",
			DBName:   "boogle",
			SSLMode:  "require",
		},
	}

	got := connectionString(conf)
	parsed, err := url.Parse(got)
	if err != nil {
		t.Fatalf("connectionString produced an unparseable URL %q: %v", got, err)
	}

	password, _ := parsed.User.Password()
	if parsed.User.Username() != "user@example.com" || password != "p@ss:w/rd" {
		t.Errorf("credentials did not round-trip: user=%q password=%q",
			parsed.User.Username(), password)
	}
	if parsed.Host != "db:5433" {
		t.Errorf("host = %q, want db:5433", parsed.Host)
	}
	if parsed.Path != "/boogle" {
		t.Errorf("dbname = %q, want /boogle", parsed.Path)
	}
	if mode := parsed.Query().Get("sslmode"); mode != "require" {
		t.Errorf("sslmode = %q, want require", mode)
	}
	if timeout := parsed.Query().Get("statement_timeout"); timeout != "30000" {
		t.Errorf("statement_timeout = %q, want 30000", timeout)
	}
}
