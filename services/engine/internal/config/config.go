package config

import (
	"github.com/Hassan-ach/boogle/services/engine/internal/config/ranker"
	"github.com/Hassan-ach/boogle/services/engine/internal/config/store"
)

type Config struct {
	Store  store.StoreConfig
	Ranker ranker.RankingConfig
}

func LoadConfig() (*Config, error) {
	c := &Config{
		Ranker: ranker.NewRankingConfig(),
		Store:  store.NewStoreConfig(),
	}

	return c, nil
}
