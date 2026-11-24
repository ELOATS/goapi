-- +goose Up
-- gen_random_uuid() 在 PostgreSQL 13+ 内置，无需扩展。
CREATE TABLE users (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text        NOT NULL CHECK (char_length(name) BETWEEN 1 AND 100),
    email      text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- 邮箱大小写不敏感唯一，和内存实现的行为一致。
CREATE UNIQUE INDEX users_email_lower_key ON users (lower(email));

-- +goose Down
DROP TABLE users;
