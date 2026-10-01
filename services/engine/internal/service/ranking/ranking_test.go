package ranking

import (
	"testing"

	"github.com/Hassan-ach/boogle/services/engine/internal/config/ranker"
	"github.com/Hassan-ach/boogle/services/engine/internal/model"
	"github.com/Hassan-ach/boogle/services/engine/internal/store"
	"github.com/Hassan-ach/boogle/services/engine/internal/util"
	"github.com/google/uuid"
)

func TestNewRankingService(t *testing.T) {
	conf := ranker.RankingConfig{}
	svc := NewRankingService(conf)
	if svc.conf != conf {
		t.Errorf("NewRankingService config mismatch")
	}
}

func TestRankingService_Rank(t *testing.T) {
	svc := NewRankingService(ranker.RankingConfig{})

	t.Run("empty dataset", func(t *testing.T) {
		wordMapper := util.NewWordMapper()
		pageMapper := util.NewPageMapper()

		data := &store.Data{
			Pages:      []*model.Page{},
			Idf:        map[string]float64{},
			WordMapper: wordMapper,
			PageMapper: pageMapper,
		}

		ranked, err := svc.Rank(data)
		if err != nil {
			t.Fatalf("Rank() unexpected error for empty data: %v", err)
		}
		if len(ranked) != 0 {
			t.Errorf("expected 0 ranked pages, got %d", len(ranked))
		}
	})

	t.Run("single page dataset", func(t *testing.T) {
		pageID := uuid.New()
		page := &model.Page{
			ID:      pageID,
			URL:     "https://single.com",
			PRScore: 0.9,
			Words:   map[string]int{"boogle": 5},
		}

		wordMapper := util.NewWordMapper()
		wordMapper.MapValue("boogle")

		pageMapper := util.NewPageMapper()
		pageMapper.MapValue(pageID)

		data := &store.Data{
			Pages:      []*model.Page{page},
			Idf:        map[string]float64{"boogle": 1.2},
			WordMapper: wordMapper,
			PageMapper: pageMapper,
		}

		ranked, err := svc.Rank(data)
		if err != nil {
			t.Fatalf("Rank() unexpected error for single page: %v", err)
		}
		if len(ranked) != 1 {
			t.Fatalf("expected 1 ranked page, got %d", len(ranked))
		}
		if ranked[0].URL != "https://single.com" {
			t.Errorf("expected URL https://single.com, got %s", ranked[0].URL)
		}
	})

	t.Run("multiple pages dataset ranking end-to-end", func(t *testing.T) {
		id1 := uuid.New()
		id2 := uuid.New()
		id3 := uuid.New()

		page1 := &model.Page{
			ID:       id1,
			URL:      "https://high-relevance.com",
			PRScore:  0.9,
			Words:    map[string]int{"search": 10, "engine": 5},
			MetaData: model.MetaData{Title: "High Relevance"},
		}
		page2 := &model.Page{
			ID:       id2,
			URL:      "https://medium-relevance.com",
			PRScore:  0.5,
			Words:    map[string]int{"search": 2, "engine": 1},
			MetaData: model.MetaData{Title: "Medium Relevance"},
		}
		page3 := &model.Page{
			ID:       id3,
			URL:      "https://low-relevance.com",
			PRScore:  0.1,
			Words:    map[string]int{"search": 1},
			MetaData: model.MetaData{Title: "Low Relevance"},
		}

		query := []string{"search", "engine"}
		wordMapper := util.NewWordMapper()
		for _, w := range query {
			wordMapper.MapValue(w)
		}

		pageMapper := util.NewPageMapper()
		pageMapper.MapValue(id1)
		pageMapper.MapValue(id2)
		pageMapper.MapValue(id3)

		idf := map[string]float64{
			"search": 1.5,
			"engine": 2.0,
		}

		data := &store.Data{
			Pages:      []*model.Page{page1, page2, page3},
			Idf:        idf,
			WordMapper: wordMapper,
			PageMapper: pageMapper,
		}

		ranked, err := svc.Rank(data)
		if err != nil {
			t.Fatalf("Rank() error: %v", err)
		}

		if len(ranked) != 3 {
			t.Fatalf("expected 3 ranked pages, got %d", len(ranked))
		}

		if ranked[0].URL != "https://high-relevance.com" {
			t.Errorf("expected first ranked page to be high-relevance, got %s", ranked[0].URL)
		}

		for _, p := range ranked {
			if p.GlobalScore < 0 {
				t.Errorf("page %s has invalid global score %f", p.URL, p.GlobalScore)
			}
		}
	})
}
