FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -o /out/camd ./cmd/camd

FROM alpine:3.19
RUN apk add --no-cache ca-certificates
COPY --from=build /out/camd /usr/local/bin/camd
ENTRYPOINT ["/usr/local/bin/camd"]
