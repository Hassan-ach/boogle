package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Hassan-ach/boogle/services/engine/internal/config"
	"github.com/Hassan-ach/boogle/services/engine/internal/service/ranking"
	"github.com/Hassan-ach/boogle/services/engine/internal/store"
)

type mockSpeller struct{}

func (s mockSpeller) GetSuggestions(q string) []string {
	return strings.Split(q, " ")
}

func main() {
	start := time.Now()
	if len(os.Args) < 2 {
		fmt.Println("missing input")
		os.Exit(1)
	}

	conf, err := config.LoadConfig()
	if err != nil {
		panic("failed to load config: " + err.Error())
	}
	store := store.NewStore(conf.Store)

	// speller, err := service.NewAspellSpellingService()
	// if err != nil {
	// 	panic("failed to initialize speller: " + err.Error())
	// }
	speller := mockSpeller{}

	ranker := ranking.NewRankingService(conf.Ranker)

	query := speller.GetSuggestions(os.Args[1])
	fmt.Printf("query: %s\n", query)
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	data, err := store.GetData(c, query, 0)
	if err != nil {
		fmt.Printf("err: %v", err)
	}
	s := time.Now()
	pages, err := ranker.Rank(data)
	fmt.Printf("ranking took: %d ms\n", time.Since(s).Abs().Milliseconds())
	if err != nil {
		fmt.Printf("err: %v", err)
	}

	for _, page := range pages {
		fmt.Printf("%s: %f  Keywords: %v\n ", page.URL, page.GlobalScore, page.MetaData.Keywords)
	}
	fmt.Printf("total execution time: %d ms\n", time.Since(start).Abs().Milliseconds())
}
