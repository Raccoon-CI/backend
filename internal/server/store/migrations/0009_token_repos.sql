-- The repositories a token of the read role may see: comma-separated patterns
-- (owner/name or owner/*, lower case). Empty means all of them, as before.
ALTER TABLE api_tokens ADD COLUMN repos TEXT NOT NULL DEFAULT '';
