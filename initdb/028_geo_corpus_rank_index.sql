-- +goose Up
-- Покривний індекс зменшує вартість читання postings до появи статистики
-- щойно створеного corpus; формула scoring та набір кандидатів не змінюються.
CREATE INDEX geo_corpus_terms_rank_idx
    ON geo_corpus_terms(run_id, domain, model, term, target_id)
    INCLUDE(weight);

-- +goose Down
DROP INDEX geo_corpus_terms_rank_idx;
