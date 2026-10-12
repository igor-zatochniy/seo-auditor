-- +goose Up
-- Cascade від audit_results потребує індексу без domain/model між ключами FK.
CREATE INDEX geo_corpus_pages_result_idx ON geo_corpus_pages(run_id, target_id);

-- +goose Down
DROP INDEX geo_corpus_pages_result_idx;
