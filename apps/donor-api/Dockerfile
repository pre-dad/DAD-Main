# syntax=docker/dockerfile:1
FROM golang:1.27.4@sha256:205c7636d74e0ffbe3d007162df7d74f859f5a8c8322dfb9f907977686c126cf AS build
WORKDIR /src
COPY apps/donor-api/go.mod apps/donor-api/go.sum ./
RUN go mod download
COPY apps/donor-api ./
RUN CGO_ENABLED=0 go build -trimpath -o /out/api ./cmd/api && \
    CGO_ENABLED=0 go build -trimpath -o /out/worker ./cmd/worker && \
    CGO_ENABLED=0 go build -trimpath -o /out/migrate ./cmd/migrate && \
    CGO_ENABLED=0 go build -trimpath -o /out/provision ./cmd/provision && \
    CGO_ENABLED=0 go build -trimpath -o /out/monitor ./cmd/monitor && \
    CGO_ENABLED=0 go build -trimpath -o /out/reconcile ./cmd/reconcile && \
    CGO_ENABLED=0 go build -trimpath -o /out/staff ./cmd/staff && \
    CGO_ENABLED=0 go build -trimpath -o /out/rekey ./cmd/rekey

FROM alpine:3.22@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8 AS runtime
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 donor
COPY --from=build /out/ /usr/local/bin/
USER donor
ENV DONOR_GO_LISTEN=0.0.0.0:8002
EXPOSE 8002
CMD ["api"]
