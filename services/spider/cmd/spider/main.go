package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/Hassan-ach/boogle/services/spider/internal/config"
	"github.com/Hassan-ach/boogle/services/spider/internal/spider"
)

type Spider struct {
	Config *config.Config
}

func main() {
	conf, _ := config.LoadConfig()

	spider := spider.NewSpider(conf)
	defer func() {
		spider.Close()
	}()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go spider.Start([]string{
		// Mathematics & Science Hubs
		"https://en.wikipedia.org/wiki/Hairy_ball_theorem",
		"https://en.wikipedia.org/wiki/Main_Page",
		"https://en.wikipedia.org/wiki/Mathematics",
		"https://en.wikipedia.org/wiki/Theoretical_computer_science",

		// High-Density Knowledge Graphs
		"https://www.wikidata.org/wiki/Wikidata:Main_Page",
		"https://archive.org/",

		// Developer & Tech Portals
		"https://github.com/trending",
		"https://stackoverflow.com/",
		"https://news.ycombinator.com/",

		// Research & Global News Hubs
		"https://news.mit.edu/",
		"https://www.bbc.com/news",
		"https://www.reuters.com",
	})
	<-sigs
	fmt.Println("Exiting gracefully")

	spider.Stop()
}
