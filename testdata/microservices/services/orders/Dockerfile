FROM golang:1.22 AS build
WORKDIR /app
COPY . .
RUN go build -o orders .

FROM gcr.io/distroless/base
COPY --from=build /app/orders /orders
ENTRYPOINT ["/orders"]
