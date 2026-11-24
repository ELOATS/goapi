# syntax=docker/dockerfile:1
# 必须不低于 go.mod 里的 go 版本（go mod tidy 可能因为依赖升级把它调高）。CI 的 Lint 会检查两者一致
ARG GO_VERSION=1.26

FROM golang:${GO_VERSION} AS build
# 要构建的命令：api（默认）或 alertbridge
ARG CMD=api
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${CMD}

# distroless：无 shell、非 root 运行，攻击面最小
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
USER nonroot:nonroot
EXPOSE 8080 9090
ENV APP_ENV=prod
ENTRYPOINT ["/app"]
