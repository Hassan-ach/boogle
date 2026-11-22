package ranking

import (
	"fmt"

	"github.com/Hassan-ach/boogle/services/engine/internal/apperror"
	"github.com/Hassan-ach/boogle/services/engine/internal/config/ranker"
	"github.com/Hassan-ach/boogle/services/engine/internal/model"
	"github.com/Hassan-ach/boogle/services/engine/internal/store"
)

type RankingService struct {
	conf ranker.RankingConfig
}

func NewRankingService(conf ranker.RankingConfig) RankingService {
	return RankingService{
		conf: conf,
	}
}

func (r RankingService) Rank(data *store.Data) ([]*model.Page, error) {
	pages, err := tfIdf(data)
	if err != nil {
		return nil, apperror.Internal(fmt.Errorf("failed to calculate TF-IDF: %w", err))
	}

	normalizeTFIDF(pages)

	// RANKER_WEIGHT_TF weights the two signals. TF-IDF is 0..1 after
	// normalizeTFIDF; PageRank is normalised separately, so neither dominates by
	// scale alone. The literal 0.5 used to ignore the configured value (M-05).
	rankedPages, err := sort(pages, r.conf.WeightTF)
	if err != nil {
		return nil, apperror.Internal(fmt.Errorf("failed to rank nodes: %w", err))
	}

	return rankedPages, nil
}
