-- +migrate Up
CREATE TABLE pool.whitelist
(
    addr varchar NOT NULL PRIMARY KEY
);

-- +migrate Down
DROP TABLE pool.whitelist;
