-- 只在数据卷第一次初始化时执行（postgres 镜像的 /docker-entrypoint-initdb.d 机制）。
-- 集成测试用独立的 app_test 库：测试会 TRUNCATE 表、留下测试数据，不能和本地环境的 app 库共用。
-- 已有的数据卷不会再执行这个文件，由 scripts/test-in-docker.ps1 和 make db-test-create 补建。
CREATE DATABASE app_test OWNER app;
