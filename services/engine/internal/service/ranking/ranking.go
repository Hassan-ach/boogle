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

	// 0.5 weights the two signals equally. TF-IDF is 0..1 after
	// normalizeTFIDF; PageRank is normalised separately, so neither dominates by
	// scale alone. Moving this changes relevance outright, not just ordering.
	rankedPages, err := sort(pages, 0.5)
	if err != nil {
		return nil, apperror.Internal(fmt.Errorf("failed to rank nodes: %w", err))
	}

	return rankedPages, nil
}
