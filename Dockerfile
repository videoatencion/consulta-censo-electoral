FROM	golang:1.20.3-alpine as build

RUN	apk update && \
	apk upgrade && \
	apk add alpine-sdk

RUN	mkdir /go/censoElectoral
COPY	main.go /go/censoElectoral
WORKDIR	/go/censoElectoral

RUN	go mod init censoElectoral  && \
	go get -u && \
	go mod tidy
RUN	CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -o censoElectoral -ldflags "-w -s" .

FROM	alpine:latest
COPY	--from=build /go/censoElectoral/censoElectoral /censoElectoral
RUN	apk update && \
	apk upgrade --no-cache && \
	apk add --no-cache sqlite-libs

USER	1000:1000

ENTRYPOINT	["/censoElectoral"]
