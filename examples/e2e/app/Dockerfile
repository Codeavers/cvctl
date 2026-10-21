FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 go build -o /app .

FROM alpine:3
RUN apk add --no-cache postgresql-client
ARG MIGRATE_FAIL=""
ARG BUILD_ID=""
ARG HEALTH_FAIL=""
ENV MIGRATE_FAIL=${MIGRATE_FAIL}
ENV BUILD_ID=${BUILD_ID}
ENV HEALTH_FAIL=${HEALTH_FAIL}
COPY --from=build /app /app
ENTRYPOINT ["/app"]
CMD ["serve"]
