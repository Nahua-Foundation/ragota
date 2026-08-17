FROM golang:1.22 AS build
WORKDIR /app
COPY . .
RUN go build -o gateway .

FROM gcr.io/distroless/base
COPY --from=build /app/gateway /gateway
ENTRYPOINT ["/gateway"]
