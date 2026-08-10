ALTER TABLE api_keys RENAME COLUMN name TO app_name;
ALTER TABLE api_keys
    ADD COLUMN username TEXT NOT NULL DEFAULT 'unknown',
    ADD COLUMN machine_name TEXT NOT NULL DEFAULT 'unknown';
ALTER TABLE api_keys
    ALTER COLUMN username DROP DEFAULT,
    ALTER COLUMN machine_name DROP DEFAULT;
