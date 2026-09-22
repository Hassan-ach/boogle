package spellchecker

type DummySpeller struct{}

func NewDummySpellingService() DummySpeller {
	return DummySpeller{}
}

func (s DummySpeller) GetSuggestions(q string) []string {
	return []string{q}
}
