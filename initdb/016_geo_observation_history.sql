-- +goose Up
CREATE TABLE geo_citation_observations (
    sequence BIGSERIAL PRIMARY KEY,
    id UUID NOT NULL UNIQUE,
    report_id UUID NOT NULL,
    ordinal INT NOT NULL,
    engine TEXT NOT NULL CHECK (engine IN ('google_ai','chatgpt','gemini','perplexity','other')),
    citation TEXT NOT NULL CHECK (citation IN ('unknown','yes','no')),
    brand_mention TEXT NOT NULL CHECK (brand_mention IN ('unknown','yes','no')),
    evidence_url TEXT NOT NULL DEFAULT '',
    note TEXT NOT NULL DEFAULT '',
    checked_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (report_id, ordinal) REFERENCES geo_query_results(report_id, ordinal) ON DELETE CASCADE
);
CREATE INDEX geo_citation_observations_query_idx ON geo_citation_observations(report_id, ordinal, sequence DESC);

-- Попередні спостереження зберігаються з початковою датою; втрачену раніше історію не вигадуємо.
INSERT INTO geo_citation_observations(id,report_id,ordinal,engine,citation,brand_mention,evidence_url,note,checked_at)
SELECT gen_random_uuid(),report_id,ordinal,engine,citation,brand_mention,evidence_url,note,checked_at
FROM geo_citation_checks ORDER BY checked_at,report_id,ordinal,engine;

-- +goose Down
-- Останні спостереження залишаються в сумісній таблиці geo_citation_checks.
DROP TABLE geo_citation_observations;
