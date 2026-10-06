FROM	golang:1.27-alpine AS build

WORKDIR	/src
COPY	go.mod go.sum ./
RUN	go mod download
COPY	*.go ./
# Pure Go SQLite driver: no CGO, fully static binary.
RUN	CGO_ENABLED=0 go test ./... && \
	CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-w -s" -o /out/censoElectoral .

FROM	gcr.io/distroless/static-debian13:nonroot
COPY	--from=build /out/censoElectoral /censoElectoral

# Keep the historical uid so existing /data volumes stay writable.
USER	1000:1000
EXPOSE	8080

# A large census can take a while to import; /health answers 503 meanwhile.
HEALTHCHECK	--interval=30s --timeout=5s --start-period=10m CMD ["/censoElectoral", "healthcheck"]

ENTRYPOINT	["/censoElectoral"]
