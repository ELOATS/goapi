-- +goose Up
-- 示例消费者用的表。真实场景里消费方通常是另一个服务、有自己的数据库，这里为了演示放在同一个库。

-- 去重：每个消费者处理过的事件。和业务写入在同一个事务里插入，
-- 所以"处理了但没记录"或"记录了但没处理"都不会发生。
CREATE TABLE processed_events (
    consumer     text        NOT NULL,
    event_id     bigint      NOT NULL,
    processed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (consumer, event_id)
);
CREATE INDEX processed_events_processed_at_idx ON processed_events (processed_at);

-- 死信：重试用完或判定为永久失败的消息，保留原始内容以便修复后重放
CREATE TABLE consumer_dead_letters (
    id         bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    consumer   text        NOT NULL,
    event_id   bigint      NOT NULL,
    event_type text        NOT NULL,
    subject    text        NOT NULL,
    headers    jsonb       NOT NULL,
    payload    bytea       NOT NULL,
    error      text        NOT NULL,
    attempts   integer     NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (consumer, event_id)
);

-- 示例业务表：收到 user.created 后记录一条待发送的欢迎通知。
-- 不加指向 users 的外键：消费方通常不和生产方共享表。
CREATE TABLE welcome_notifications (
    user_id    uuid        PRIMARY KEY,
    email      text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE welcome_notifications;
DROP TABLE consumer_dead_letters;
DROP TABLE processed_events;
