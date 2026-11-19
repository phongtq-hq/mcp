# syntax=docker/dockerfile:1
# Một image cho mọi binary của platform (api, gateway, worker, migrate, hqadmin và các
# managed MCP server). Mỗi service chọn binary của nó bằng `command`. Image không chứa
# cấu hình hay khoá nào: mọi thứ đến lúc chạy, qua biến môi trường và file được mount.
ARG GO_VERSION=1.27
ARG NODE_VERSION=24

# Giao diện web (ADR-0016): build thành file tĩnh, rồi được nhúng vào binary api ở stage Go.
# Node chỉ có ở stage này; image cuối không chứa Node hay node_modules.
FROM node:${NODE_VERSION}-alpine AS ui
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
# --ignore-scripts: không gói nào cần script cài đặt; mã của phụ thuộc không chạy lúc cài.
RUN --mount=type=cache,target=/root/.npm npm ci --ignore-scripts
COPY web ./
# Vite ghi vào ../internal/webui/dist (xem web/vite.config.ts).
RUN npm run build

FROM golang:${GO_VERSION}-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY --from=ui /src/internal/webui/dist ./internal/webui/dist
COPY migrations ./migrations
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -ldflags='-s -w' -o /out/ ./cmd/...

# distroless static: không shell, không trình quản lý gói; có CA để gọi provider qua TLS.
# Chạy bằng user không phải root (65532) trừ khi service đặt `user` khác.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /usr/local/bin/
USER nonroot:nonroot
# Không có ENTRYPOINT mặc định có nghĩa: service nào cũng phải nêu binary của mình.
ENTRYPOINT []
CMD ["/usr/local/bin/api"]
