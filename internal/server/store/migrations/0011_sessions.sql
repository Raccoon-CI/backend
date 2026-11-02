-- A token made by signing in with GitHub is a session: it names the user and
-- expires. Tokens made through the API have neither (user '' and expires_at 0).
ALTER TABLE api_tokens ADD COLUMN user TEXT NOT NULL DEFAULT '';
ALTER TABLE api_tokens ADD COLUMN expires_at INTEGER NOT NULL DEFAULT 0; -- unix milliseconds; 0 = never
