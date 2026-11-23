package ranker

import "github.com/Hassan-ach/boogle/services/engine/internal/util"

// RankingConfig holds the blend weight only. RANKER_MAX_RESULTS is the store's
// candidate window (store.CandidateLimit), read there so there is one source of
// truth for how many results exist.
type RankingConfig struct {
	WeightTF float64
}

func NewRankingConfig() RankingConfig {
	return RankingConfig{
		WeightTF: util.GetFloatWithDefault("RANKER_WEIGHT_TF", 0.5),
	}
}
