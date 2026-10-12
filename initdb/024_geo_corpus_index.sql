-- +goose Up
ALTER TABLE geo_reports DROP CONSTRAINT geo_reports_page_count_check;
ALTER TABLE geo_reports ADD CONSTRAINT geo_reports_page_count_check CHECK (page_count > 0);
ALTER TABLE geo_reports ADD COLUMN corpus JSONB;

CREATE TABLE geo_corpora (
    run_id UUID NOT NULL REFERENCES audit_runs(id) ON DELETE CASCADE,
    domain TEXT NOT NULL,
    model TEXT NOT NULL,
    source_pages INT NOT NULL DEFAULT 0 CHECK (source_pages >= 0),
    page_count INT NOT NULL DEFAULT 0 CHECK (page_count >= 0),
    limited_pages INT NOT NULL DEFAULT 0 CHECK (limited_pages >= 0),
    ready BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (run_id, domain, model)
);

CREATE TABLE geo_corpus_pages (
    run_id UUID NOT NULL,
    domain TEXT NOT NULL,
    model TEXT NOT NULL,
    target_id BIGINT NOT NULL,
    phrases TEXT[] NOT NULL,
    limited BOOLEAN NOT NULL,
    PRIMARY KEY (run_id, domain, model, target_id),
    FOREIGN KEY (run_id, domain, model) REFERENCES geo_corpora ON DELETE CASCADE,
    FOREIGN KEY (run_id, target_id) REFERENCES audit_results(run_id, target_id) ON DELETE CASCADE,
    CHECK (cardinality(phrases) <= 128)
);

CREATE TABLE geo_corpus_terms (
    run_id UUID NOT NULL,
    domain TEXT NOT NULL,
    model TEXT NOT NULL,
    target_id BIGINT NOT NULL,
    term TEXT NOT NULL CHECK (char_length(term) BETWEEN 2 AND 64),
    weight SMALLINT NOT NULL CHECK (weight BETWEEN 1 AND 4),
    PRIMARY KEY (run_id, domain, model, term, target_id),
    FOREIGN KEY (run_id, domain, model, target_id) REFERENCES geo_corpus_pages ON DELETE CASCADE
);
CREATE INDEX geo_corpus_terms_target_idx ON geo_corpus_terms(run_id, domain, model, target_id);

CREATE TABLE geo_corpus_dictionary (
    run_id UUID NOT NULL,
    domain TEXT NOT NULL,
    model TEXT NOT NULL,
    term TEXT NOT NULL,
    family TEXT NOT NULL,
    first_rune TEXT NOT NULL,
    length SMALLINT NOT NULL,
    fuzzy BOOLEAN NOT NULL,
    PRIMARY KEY (run_id, domain, model, term),
    FOREIGN KEY (run_id, domain, model) REFERENCES geo_corpora ON DELETE CASCADE
);
CREATE INDEX geo_corpus_dictionary_family_idx ON geo_corpus_dictionary(run_id, domain, model, family, term);
CREATE INDEX geo_corpus_dictionary_fuzzy_idx ON geo_corpus_dictionary(run_id, domain, model, first_rune, length, term) WHERE fuzzy;

-- +goose Down
-- Старі великі звіти зберігаються: rollback індексу не повертає обмеження 1000.
DROP TABLE geo_corpus_dictionary;
DROP TABLE geo_corpus_terms;
DROP TABLE geo_corpus_pages;
DROP TABLE geo_corpora;
ALTER TABLE geo_reports DROP COLUMN corpus;
