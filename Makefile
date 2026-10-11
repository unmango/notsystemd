GO_SRC ?= $(shell find . -name '*.go')

build:
	nix build .#

bin/notsystemd: ${GO_SRC} go.mod go.sum
	CGO_ENABLED=0 go build -o $@ ./cmd/notsystemd

test:
	go tool ginkgo run -r

check lint:
	nix flake check

format fmt:
	nix fmt

update:
	nix flake update

tidy: go.sum nix/gomod2nix.toml

go.sum: go.mod ${GO_SRC}
	go mod tidy

nix/gomod2nix.toml: go.sum
	gomod2nix generate --dir ${CURDIR} --outdir ${@D}

.PHONY: build test check lint format fmt update tidy
