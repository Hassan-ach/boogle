package store

import "github.com/Hassan-ach/boogle/services/engine/internal/util"

type StoreConfig struct {
	DB       PsqlConfig
	PageSize int
	// CandidateLimit is how many matches the SQL pass hands to the ranker. The
	// blend has to happen before the page is sliced, otherwise page 1 is chosen
	// by the coarse SQL order (word count) and a globally better result can
	// never surface (M-03). RANKER_MAX_RESULTS is the upper bound a user can page
	// through.
	CandidateLimit int
}

func NewStoreConfig() StoreConfig {
	pageSize := util.GetIntWithDefault("PAGE_SIZE", 20)
	if pageSize < 1 {
		// LIMIT 0 returns an empty page with HTTP 200, which looks like a
		// working search that found nothing.
		pageSize = 20
	}

	candidateLimit := util.GetIntWithDefault("RANKER_MAX_RESULTS", 100)
	if candidateLimit < pageSize {
		candidateLimit = pageSize
	}

	return StoreConfig{
		DB:             NewDatabaseConfig(),
		PageSize:       pageSize,
		CandidateLimit: candidateLimit,
	}
}
