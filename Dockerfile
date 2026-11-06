# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26.4-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
RUN apk add --no-cache git
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/pinglens ./cmd/pinglens

FROM alpine:3.21
COPY --from=build /out/pinglens /usr/local/bin/pinglens
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/pinglens"]
