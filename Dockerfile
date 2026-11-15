FROM golang:1.24-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /distro .

FROM alpine:3.20
RUN adduser -D -u 10001 distro && mkdir /data && chown distro /data
USER distro
COPY --from=build /distro /distro
ENV PORT=8080 DATA_DIR=/data
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/distro"]
