FROM golang:1.25 AS builder
WORKDIR /src
COPY . .
RUN go build -o /out/app ./cmd/app

FROM gcr.io/distroless/base-debian12 AS runtime
COPY --from=builder /out/app /app
EXPOSE 8080
ENTRYPOINT ["/app"]
