# 本地 Keycloak

只用于本地验证认证，**不要用于任何共享环境**：开发模式（`start-dev`）、HTTP、用户密码和用户名相同、管理员是 admin / admin。

`goapi-realm.json` 在容器第一次启动时导入（`--import-realm`），之后改了这个文件要删掉容器重建才会生效：
`docker compose -f docker-compose.yml -f docker-compose.auth.yml up -d --force-recreate keycloak`。

| 用户 | 密码 | 角色 | 预期 |
|---|---|---|---|
| alice | alice | goapi-admin | 能读能写 |
| bob | bob | goapi-viewer | 只能读，写返回 403 |
| carol | carol | （无） | 能调 `/api/v1/me`，其它接口返回 403 |

客户端 `goapi-cli` 是公开客户端，只开了密码模式（Direct Access Grants），用来在命令行拿 token；
它带一个 audience mapper，让 access token 的 `aud` 包含 `goapi`（api 的 `OIDC_AUDIENCE`）。
角色在 token 的 `realm_access.roles` 里（api 的 `OIDC_ROLES_CLAIM`）。

管理后台：http://localhost:8180/admin
