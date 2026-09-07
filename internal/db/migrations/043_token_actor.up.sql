-- Who minted this credential, and who revoked it. specs/api-auth.md section 7.
--
-- TWO columns because a token has two attributable events and neither supersedes the other --
-- unlike a definition (created once) or a channel (moved repeatedly, last mover wins). Each
-- pairs with the timestamp of its own write: actor with created_at, revoked_by with revoked_at.
--
-- Empty is "unattributed", which is every row written before this. Nothing writes it now --
-- each mint names its path, down to `startup:auto-mint` for the one genroc generates itself.
ALTER TABLE api_tokens ADD COLUMN actor      TEXT NOT NULL DEFAULT '';
ALTER TABLE api_tokens ADD COLUMN revoked_by TEXT NOT NULL DEFAULT '';
