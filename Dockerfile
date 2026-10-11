# ---- build stage ----
FROM golang:1.26-alpine AS build
WORKDIR /src

RUN apk add --no-cache git

# Cache module downloads separately from source changes
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Pin templ to the exact version this module was built against
RUN go install github.com/a-h/templ/cmd/templ@v0.3.977
RUN templ generate
RUN CGO_ENABLED=0 go build -trimpath -o /out/app ./cmd/api

# ---- runtime stage ----
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -H appuser
WORKDIR /app
COPY --from=build /out/app ./app
COPY --from=build /src/web ./web
# Client-uploaded model files live here; mounted as a named volume so they
# survive rebuilds (the volume inherits this ownership on first use).
RUN mkdir -p /app/data/uploads && chown -R appuser /app/data
USER appuser

ENTRYPOINT ["./app"]
