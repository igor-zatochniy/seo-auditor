-- +goose Up
ALTER TABLE geo_profiles
    ADD COLUMN desired_ai_policy JSONB NOT NULL DEFAULT '{}'::jsonb
    CHECK (jsonb_typeof(desired_ai_policy) = 'object' AND octet_length(desired_ai_policy::TEXT) <= 4096);

UPDATE geo_profiles
SET desired_ai_policy = jsonb_build_object(
    'googlebot', 'unknown',
    'google-extended', 'unknown',
    'oai-searchbot', 'unknown',
    'gptbot', 'unknown',
    'claude-searchbot', 'unknown',
    'claudebot', 'unknown',
    'perplexitybot', 'unknown',
    'applebot', 'unknown',
    'applebot-extended', 'unknown'
);

ALTER TABLE geo_profile_runs
    ADD COLUMN desired_ai_policy JSONB NOT NULL DEFAULT '{}'::jsonb
    CHECK (jsonb_typeof(desired_ai_policy) = 'object' AND octet_length(desired_ai_policy::TEXT) <= 4096);

UPDATE geo_profile_runs AS run
SET desired_ai_policy = profile.desired_ai_policy
FROM geo_profiles AS profile
WHERE profile.id = run.profile_id;

-- +goose Down
ALTER TABLE geo_profile_runs DROP COLUMN desired_ai_policy;
ALTER TABLE geo_profiles DROP COLUMN desired_ai_policy;
