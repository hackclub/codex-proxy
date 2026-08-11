ALTER TABLE tokens
    ADD COLUMN plan_type TEXT,
    ADD COLUMN active_limit TEXT,
    ADD COLUMN primary_used_percent DOUBLE PRECISION,
    ADD COLUMN primary_reset_at TIMESTAMPTZ,
    ADD COLUMN secondary_used_percent DOUBLE PRECISION,
    ADD COLUMN secondary_reset_at TIMESTAMPTZ,
    ADD COLUMN limited_until TIMESTAMPTZ,
    ADD COLUMN limit_reason TEXT;

CREATE INDEX idx_tokens_limited_until ON tokens(limited_until)
    WHERE enabled = true AND healthy = true;
