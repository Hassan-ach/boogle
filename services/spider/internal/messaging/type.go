package messaging

type IndexerJob struct {
	PageId string `json:"page_id"`
}

func NewIndexerJobPayload(pageId string) []byte {
	jsonPayload := []byte(`{"page_id":"` + pageId + `"}`)
	return jsonPayload
}
