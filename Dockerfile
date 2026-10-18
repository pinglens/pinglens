# syntax=docker/dockerfile:1
FROM golang:1.26.4-alpine AS build
WORKDIR /src
RUN apk add --no-cache git
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/pinglens ./cmd/pinglens

FROM alpine:3.21
RUN apk add --no-cache ca-certificates
COPY --from=build /out/pinglens /usr/local/bin/pinglens
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/pinglens"]
