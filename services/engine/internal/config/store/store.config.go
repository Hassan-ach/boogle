package store

import "github.com/Hassan-ach/boogle/services/engine/internal/util"

type StoreConfig struct {
	DB       PsqlConfig
	PageSize int
	// CandidateLimit is how many matches the SQL pass hands to the ranker. The
	// blend must happen before slicing the page so the best results surface first;
	// RANKER_MAX_RESULTS is the upper bound a user can page through.
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
