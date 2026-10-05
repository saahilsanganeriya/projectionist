IMAGE   ?= projectionist
VERSION ?= $(shell git describe --always --dirty 2>/dev/null || echo dev)
export GOFLAGS = -p=4 -trimpath

.PHONY: test test-linux vet build image save

test:
	go test ./...

# Same tests on linux/amd64 (renameat2, FICLONE paths) in a throwaway golang container.
test-linux:
	docker run --rm --platform linux/amd64 -e GOFLAGS="$(GOFLAGS)" -v "$(CURDIR)":/src -w /src golang:1.26 go test ./...

vet:
	go vet ./...
	GOOS=linux GOARCH=amd64 go vet ./...

build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-s -w -X main.version=$(VERSION)" -o bin/projectionist ./cmd/projectionist

image:
	docker buildx build --platform linux/amd64 --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) -t $(IMAGE):latest --load .

# Tarball for `docker load` on the NAS (no registry).
save: image
	mkdir -p bin && docker save $(IMAGE):$(VERSION) | gzip > bin/$(IMAGE)-$(VERSION).tar.gz
