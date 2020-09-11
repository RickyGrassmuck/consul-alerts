FROM golang:1.15 AS golang
ENV GOPATH /go
COPY . /go/src/consul-alerts 
WORKDIR /go/src/consul-alerts 
RUN go mod tidy
RUN go build -o /go/bin/consul-alerts .

FROM gcr.io/distroless/base-debian10
WORKDIR /bin/
COPY --from=golang /go/bin/consul-alerts .
EXPOSE 9000
CMD []
ENTRYPOINT [ "/bin/consul-alerts", "--alert-addr=0.0.0.0:9000" ]