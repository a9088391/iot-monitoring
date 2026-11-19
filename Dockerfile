FROM node:24.22.2-alpine3.24@sha256:a34ab6d7f6f4813f4d0c5469d74685604b962fbdbf5793a7bc7b4998ecb867c8 AS frontend
WORKDIR /src
# 依賴清單先於原始碼複製，修改應用時可沿用依賴安裝層。
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY frontend/ ./
RUN npm run typecheck && npm run build

FROM golang:1.27.3-alpine3.24@sha256:fa3d596898d9f126af7c01d14ee7597bab3f39cccf392ef36b69fe77cd681f18 AS backend
ENV GOTOOLCHAIN=local
WORKDIR /src
COPY backend/go.mod backend/go.sum ./
RUN go mod download
COPY backend/ ./
RUN CGO_ENABLED=0 go build -trimpath -o /out/iot .

# race 檢查只在專用建置目標加入 C 工具鏈，正式映像仍使用原本的靜態執行檔。
FROM backend AS backend-test
RUN apk add --no-cache gcc musl-dev
RUN CGO_ENABLED=1 go test -race -c -o /out/measurements.test . \
    && CGO_ENABLED=1 go build -race -trimpath -o /out/reset-app .

# 執行階段只帶入 Go 執行檔與前端產物，讓兩者由同一個服務提供。
FROM alpine:3.24.0@sha256:a2d49ea686c2adfe3c992e47dc3b5e7fa6e6b5055609400dc2acaeb241c829f4
WORKDIR /app
COPY --from=backend /out/iot /app/iot
COPY --from=frontend /src/dist /app/web
EXPOSE 8080
ENTRYPOINT ["/app/iot"]
