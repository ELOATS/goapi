-- +goose Up
-- 可空列：向后兼容。滚动发布期间旧版本不写这一列，回滚到旧版本也能正常运行。
ALTER TABLE outbox_events ADD COLUMN actor text;

-- +goose Down
ALTER TABLE outbox_events DROP COLUMN actor;
