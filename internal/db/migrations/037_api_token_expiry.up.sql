-- An expiry for tokens that stand in for a session rather than for a machine.
--
-- A browser trades its proxy identity for a bearer token, and the exchange cannot hand back one
-- it issued before -- only the hash is stored, so the plaintext is gone. Every exchange therefore
-- mints, and without an expiry each page load left another permanent credential behind.
--
-- NULL means "never expires", which is what a machine credential wants: rotating a worker token
-- is a deploy, not a clock.
ALTER TABLE api_tokens ADD COLUMN expires_at BIGINT;
