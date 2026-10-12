-- +goose Up
CREATE TABLE geo_ai_answers (
    id UUID PRIMARY KEY REFERENCES geo_citation_observations(id) ON DELETE CASCADE,
    payload_hash BYTEA NOT NULL CHECK (octet_length(payload_hash)=32),
    answer_text TEXT NOT NULL CHECK (octet_length(answer_text)<=100000),
    sources_text TEXT NOT NULL CHECK (octet_length(sources_text)<=20000),
    analysis JSONB NOT NULL CHECK (jsonb_typeof(analysis)='object'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- +goose Down
-- Історія підтверджених цитувань залишається; видаляються лише деталі відповідей.
DROP TABLE geo_ai_answers;
