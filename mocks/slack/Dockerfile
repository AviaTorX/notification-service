FROM golang:1.22-alpine AS build
WORKDIR /src
COPY main.go .
RUN go build -o /out/slack-mock main.go

FROM alpine:3.20
COPY --from=build /out/slack-mock /usr/local/bin/slack-mock
EXPOSE 9000
ENTRYPOINT ["/usr/local/bin/slack-mock"]
