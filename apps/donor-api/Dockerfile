# syntax=docker/dockerfile:1
FROM golang:1.27.4@sha256:205c7636d74e0ffbe3d007162df7d74f859f5a8c8322dfb9f907977686c126cf AS build
WORKDIR /src
COPY apps/donor-api/go.mod apps/donor-api/go.sum ./
RUN go mod download
COPY apps/donor-api ./
ARG BUILD_REVISION=unknown
RUN for command in api worker migrate provision monitor reconcile staff rekey; do \
      CGO_ENABLED=0 go build -trimpath \
        -ldflags="-X dad/donor-api/internal/buildinfo.commit=${BUILD_REVISION}" \
        -o "/out/$command" "./cmd/$command" || exit 1; \
    done

FROM alpine:3.22@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8 AS runtime
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 donor
COPY --from=build /out/ /usr/local/bin/
COPY apps/donor-api/internal/partnerreport/fonts/OFL-*.txt /usr/share/licenses/dad-report-fonts/
COPY apps/donor-api/internal/partnerreport/THIRD_PARTY_LICENSES.txt /usr/share/licenses/dad-report-renderer/
USER donor
ENV DONOR_GO_LISTEN=0.0.0.0:8002
EXPOSE 8002
CMD ["api"]
