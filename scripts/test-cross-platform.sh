#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root"

go test ./... -count=1
go test -race ./... -count=1
go vet ./...
go build ./cmd/codexfold

CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/codexfold-linux-amd64 ./cmd/codexfold
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o /tmp/codexfold-windows-amd64.exe ./cmd/codexfold
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -c -o /tmp/codexfold-testfs-linux.test ./internal/testfs
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go test -c -o /tmp/codexfold-testfs-windows.test.exe ./internal/testfs
