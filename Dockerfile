FROM golang:1.22-alpine AS build
WORKDIR /src
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/notifd ./cmd/notifd

FROM alpine:3.20
RUN apk add --no-cache ca-certificates curl
COPY --from=build /out/notifd /usr/local/bin/notifd
COPY migrations /migrations
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/notifd"]
CMD ["api"]
