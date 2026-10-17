FROM node:26-alpine AS web-build
WORKDIR /src/web
RUN npm install --global pnpm@11.25.0
COPY web/package.json web/pnpm-lock.yaml web/pnpm-workspace.yaml ./
RUN pnpm install --frozen-lockfile
COPY web/ ./
RUN pnpm run build

FROM golang:1.27.2-alpine AS go-build
WORKDIR /src
ENV GOWORK=off
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /next-game-crush ./cmd/next-game-crush

FROM alpine:3.22
RUN apk add --no-cache ca-certificates && addgroup -S app && adduser -S -G app app
WORKDIR /app
COPY --from=go-build /next-game-crush ./next-game-crush
COPY --from=web-build /src/web/dist ./web/dist
USER app
EXPOSE 8080
CMD ["./next-game-crush"]
