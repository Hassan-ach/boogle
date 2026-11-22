package store

import "testing"

func TestAPageSizeBelowOneFallsBackInsteadOfReturningEmptyPages(t *testing.T) {
	t.Setenv("PAGE_SIZE", "0")
	t.Setenv("RANKER_MAX_RESULTS", "100")

	conf := NewStoreConfig()

	if conf.PageSize != 20 {
		t.Errorf("PageSize = %d, want 20", conf.PageSize)
	}
}

func TestTheCandidateWindowIsAtLeastOnePage(t *testing.T) {
	t.Setenv("PAGE_SIZE", "20")
	t.Setenv("RANKER_MAX_RESULTS", "5")

	conf := NewStoreConfig()

	if conf.CandidateLimit != 20 {
		t.Errorf("CandidateLimit = %d, want at least PageSize (20)", conf.CandidateLimit)
	}
}

func TestTheCandidateWindowComesFromRankerMaxResults(t *testing.T) {
	t.Setenv("PAGE_SIZE", "20")
	t.Setenv("RANKER_MAX_RESULTS", "75")

	conf := NewStoreConfig()

	if conf.CandidateLimit != 75 {
		t.Errorf("CandidateLimit = %d, want 75", conf.CandidateLimit)
	}
}
