-- +migrate Up
CREATE TABLE pool.specialed (
    addr VARCHAR PRIMARY KEY
);

-- +migrate Down
DROP TABLE pool.specialed;
