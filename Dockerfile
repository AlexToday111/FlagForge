FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/flag-server ./cmd/flag-server && \
    CGO_ENABLED=0 go build -trimpath -o /out/flagctl ./cmd/flag-cli && \
    CGO_ENABLED=0 go build -trimpath -o /out/example-service ./cmd/example-service
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /usr/local/bin/
EXPOSE 8080 8081
ENTRYPOINT ["/usr/local/bin/flag-server"]
