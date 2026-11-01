IMAGE ?= arena-python:3.12

.PHONY: build images test integration lint redteam bench

build:
	go build -o bin/arena ./cmd/arena

images:
	docker build -t $(IMAGE) images/python

test:
	go test -race -count=1 ./...

integration: images
	go test -race -count=1 -tags integration -timeout 20m ./...

lint:
	test -z "$$(gofmt -l .)"
	go vet ./... && go vet -tags integration ./...
	go run honnef.co/go/tools/cmd/staticcheck@latest ./...

redteam: build images
	./bin/arena redteam -out docs/REDTEAM.md

bench: build images
	./bin/arena bench
