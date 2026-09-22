CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE urls (
    id UUID DEFAULT gen_random_uuid() PRIMARY KEY,
    url TEXT UNIQUE NOT NULL
    -- status TEXT NOT NULL CHECK (status IN ('pending', 'crawled', 'failed')),
);

CREATE TABLE pages (
    id UUID DEFAULT gen_random_uuid() PRIMARY KEY,
    url_id UUID UNIQUE NOT NULL REFERENCES urls(id) ON DELETE CASCADE,
    html TEXT NOT NULL,
    metadata JSONB NOT NULL DEFAULT '{}',
    indexed BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMP NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP NOT NULL DEFAULT NOW()
);

CREATE TABLE words (
    id UUID DEFAULT gen_random_uuid() PRIMARY KEY,
    word VARCHAR(25) UNIQUE NOT NULL,
    idf DOUBLE PRECISION NOT NULL DEFAULT 1,  
    doc_frequency INTEGER DEFAULT 0
);

CREATE TABLE page_word (
    page_id UUID NOT NULL REFERENCES pages(id) ON DELETE CASCADE,
    word_id UUID NOT NULL REFERENCES words(id) ON DELETE CASCADE,
    tf INTEGER NOT NULL CHECK (tf > 0),
    PRIMARY KEY (page_id, word_id)
);

CREATE TABLE graph_edges (
    id BIGSERIAL PRIMARY KEY,
    from_url UUID NOT NULL REFERENCES urls(id) ON DELETE CASCADE,
    to_url   UUID NOT NULL REFERENCES urls(id) ON DELETE CASCADE,
    UNIQUE (from_url, to_url)
);

CREATE TABLE page_rank (
    url_id UUID PRIMARY KEY REFERENCES urls(id) ON DELETE CASCADE,
    score   DOUBLE PRECISION NOT NULL,
    updated_at TIMESTAMP NOT NULL DEFAULT NOW()
);

-- Monitor orchestrator state
CREATE TABLE IF NOT EXISTS monitor_state (
  key TEXT PRIMARY KEY,
  value BIGINT NOT NULL,
  updated_at TIMESTAMP NOT NULL DEFAULT NOW()
);

-- CREATE TABLE image_page (
--     image_url TEXT NOT NULL,
--     page_id  UUID NOT NULL,
--     PRIMARY KEY (image_url, page_id),
--     FOREIGN KEY (page_id) REFERENCES pages(id) ON DELETE CASCADE
-- );


-- Indexes
CREATE UNIQUE INDEX idx_words_word ON words(word);
CREATE UNIQUE INDEX idx_pages_url_id ON pages(url_id);
CREATE INDEX idx_page_word_page_id ON page_word(page_id);
CREATE INDEX idx_page_word_word_id ON page_word(word_id);
CREATE UNIQUE INDEX idx_page_word_word_page ON page_word(word_id, page_id);
CREATE INDEX idx_graph_edges_from_page ON graph_edges(from_url);
CREATE INDEX idx_graph_edges_to_page ON graph_edges(to_url);
CREATE UNIQUE INDEX idx_graph_edges_unique ON graph_edges(from_url, to_url);
CREATE UNIQUE INDEX idx_graph_edges_unique_revese ON graph_edges(to_url, from_url);
CREATE INDEX idx_page_rank_score ON page_rank(score DESC);

-- CREATE INDEX idx_image_page_image_url ON image_page(image_url);
--
-- CREATE INDEX idx_image_page_page_id ON image_page(page_id);
