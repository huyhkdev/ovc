.PHONY: build test vet dist
# The OVC Server the CLI talks to is built in (devs do not configure it):
#   make dist SERVER=https://ovc.company.local
SERVER ?=
LDFLAGS := -X ovc/internal/cli.DefaultServer=$(SERVER)
build:
	go build -ldflags "$(LDFLAGS)" -o bin/ ./cmd/...
test:
	go test ./...
vet:
	go vet ./...
dist:
	GOOS=windows GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/ovc-windows-amd64.exe ./cmd/ovc
	GOOS=linux   GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/ovc-linux-amd64 ./cmd/ovc
	GOOS=darwin  GOARCH=arm64 go build -ldflags "$(LDFLAGS)" -o dist/ovc-darwin-arm64 ./cmd/ovc
	GOOS=darwin  GOARCH=amd64 go build -ldflags "$(LDFLAGS)" -o dist/ovc-darwin-amd64 ./cmd/ovc
