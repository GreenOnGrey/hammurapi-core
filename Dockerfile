# Hammurapi core image: one static binary with modes
# api | worker | cleaner | migrate.

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/hammurapi ./cmd/hammurapi \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/fakegitlab ./cmd/fakegitlab

# Release image (docker build --target release): one static binary — api,
# worker, cleaner and migrate. The agent lives in Nabu (FTR.HMR.CMN-0007 R3):
# no Node.js and no Pi here.
FROM gcr.io/distroless/static-debian12:nonroot AS release
ARG VERSION=dev
COPY --from=build /out/hammurapi /usr/local/bin/hammurapi
LABEL org.opencontainers.image.title="hammurapi-core" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.source="https://github.com/GreenOnGrey/hammurapi-core"
# Numeric user (distroless nonroot): Kubernetes checks runAsNonRoot only for numeric users.
USER 65532:65532
EXPOSE 8080 8083 9100
ENTRYPOINT ["/usr/local/bin/hammurapi"]
CMD ["api"]

# Development/test only: in-memory imitation of the GitLab API (docker build --target fakegitlab).
FROM gcr.io/distroless/static-debian12:nonroot AS fakegitlab
COPY --from=build /out/fakegitlab /usr/local/bin/fakegitlab
EXPOSE 8929
ENTRYPOINT ["/usr/local/bin/fakegitlab"]

# The default target is the release image.
FROM release
