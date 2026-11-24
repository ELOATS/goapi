-- +goose Up
-- 全部是可空列或带默认值的列：旧版本 API 不写它们也能正常插入。
ALTER TABLE outbox_events
    ADD COLUMN trace_context jsonb,                    -- 写入时的 W3C traceparent，relay 用它把链路接上
    ADD COLUMN attempts      integer NOT NULL DEFAULT 0,
    ADD COLUMN last_error    text,
    ADD COLUMN dead_at       timestamptz;              -- 不为空表示判定为无法投递（死信），不再重试

-- 待投递队列：只索引未投递且不是死信的事件
DROP INDEX outbox_events_unpublished_idx;
CREATE INDEX outbox_events_pending_idx ON outbox_events (id)
    WHERE published_at IS NULL AND dead_at IS NULL;

-- 清理已投递的旧事件
CREATE INDEX outbox_events_published_at_idx ON outbox_events (published_at)
    WHERE published_at IS NOT NULL;

-- 有新事件时通知 relay，降低延迟。NOTIFY 在事务提交时才发出，同一事务内会合并成一条。
-- +goose StatementBegin
CREATE FUNCTION outbox_events_notify() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM pg_notify('outbox_events', '');
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER outbox_events_notify AFTER INSERT ON outbox_events
    FOR EACH STATEMENT EXECUTE FUNCTION outbox_events_notify();

-- +goose Down
DROP TRIGGER outbox_events_notify ON outbox_events;
DROP FUNCTION outbox_events_notify();
DROP INDEX outbox_events_published_at_idx;
DROP INDEX outbox_events_pending_idx;
CREATE INDEX outbox_events_unpublished_idx ON outbox_events (id) WHERE published_at IS NULL;
ALTER TABLE outbox_events
    DROP COLUMN dead_at,
    DROP COLUMN last_error,
    DROP COLUMN attempts,
    DROP COLUMN trace_context;
