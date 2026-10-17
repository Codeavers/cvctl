FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 go build -o /app .

FROM alpine:3
ARG MIGRATE_FAIL=""
ARG BUILD_ID=""
ENV MIGRATE_FAIL=${MIGRATE_FAIL}
ENV BUILD_ID=${BUILD_ID}
COPY --from=build /app /app
ENTRYPOINT ["/app"]
CMD ["serve"]
