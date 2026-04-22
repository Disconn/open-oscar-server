-- Migration 0028 re-added icq_permissions_authRequired with DEFAULT true
-- ("require authorization before others may add me"). FeedbagService.UpsertItem
-- then skips inserting buddy rows for those UINs until the client completes an
-- authorization exchange (SNAC status 0x000E), which breaks normal buddy adds.
--
-- Clear the flag for all existing users. New registrations set the column
-- explicitly to false in InsertUser (SQLite here may not support ALTER COLUMN
-- SET DEFAULT).

UPDATE users
SET icq_permissions_authRequired = false;
