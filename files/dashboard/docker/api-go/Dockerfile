FROM golang:1.23-alpine AS build
WORKDIR /src
COPY docker/api-go/ ./
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go mod tidy && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/lab-api .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -u 10001 app
WORKDIR /app
COPY --from=build /out/lab-api /app/lab-api
COPY docker/api/static /app/static
USER app
EXPOSE 8000
ENTRYPOINT ["/app/lab-api"]
