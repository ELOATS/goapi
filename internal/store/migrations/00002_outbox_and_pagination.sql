-- +goose Up
-- keyset 分页索引：和 ORDER BY created_at DESC, id DESC 完全一致。
CREATE INDEX users_created_at_id_idx ON users (created_at DESC, id DESC);

-- transactional outbox：与业务数据同事务写入，由独立的 relay 进程投递到 MQ。
CREATE TABLE outbox_events (
    id           bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    aggregate_id uuid        NOT NULL,
    event_type   text        NOT NULL,
    payload      jsonb       NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz
);

-- 只索引未投递的事件，relay 轮询时很便宜。
CREATE INDEX outbox_events_unpublished_idx ON outbox_events (id) WHERE published_at IS NULL;

-- +goose Down
DROP TABLE outbox_events;
DROP INDEX users_created_at_id_idx;
