-- +migrate Up
ALTER TABLE pool.transaction ADD COLUMN priority BIGINT DEFAULT 0;
-- +migrate Down
ALTER TABLE pool.transaction DROP COLUMN priority;

