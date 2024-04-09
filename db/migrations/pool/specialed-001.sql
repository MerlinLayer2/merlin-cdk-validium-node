-- +migrate Up
CREATE TABLE pool.specialed (
    addr VARCHAR PRIMARY KEY
);

ALTER TABLE pool.transaction ADD COLUMN priority BIGINT DEFAULT 0;

-- +migrate Down
DROP TABLE pool.specialed;

ALTER TABLE pool.transaction DROP COLUMN priority;