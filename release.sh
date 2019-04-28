
CC=/usr/local/bin/musl-gcc CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o consul-alerts -a -ldflags '-extldflags "-static" -s -w'  .
