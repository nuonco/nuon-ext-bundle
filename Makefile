BINARY := nuon-ext-bundle

.DEFAULT_GOAL := build

.PHONY: build test fmt vet clean

build:
	go build -o "$(BINARY)" .

test:
	go test ./...

fmt:
	go fmt ./...

vet:
	go vet ./...

clean:
	rm -f "$(BINARY)"
