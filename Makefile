.DEFAULT_GOAL := all
all: generate-modelpb generate gomodtidy update-licenses fieldalignment fmt protolint

test:
	go test -v ./...

fmt:
	go run golang.org/x/tools/cmd/goimports@v0.3.0 -w .

protolint:
	docker run --volume "$(PWD):/workspace" --workdir /workspace bufbuild/buf lint model/proto

gomodtidy:
	go mod tidy -v

generate:
	go generate ./...

fieldalignment:
	go run golang.org/x/tools/go/analysis/passes/fieldalignment/cmd/fieldalignment@v0.4.0 -test=false $(shell go list ./... | grep -v modeldecoder/generator | grep -v test | grep -v model/modelpb)

update-licenses:
	go run github.com/elastic/go-licenser@v0.4.1 -ext .go .
	go run github.com/elastic/go-licenser@v0.4.1 -ext .proto .

install-protobuf:
	./tools/install-protobuf.sh

generate-modelpb: install-protobuf
	./tools/generate-modelpb.sh
