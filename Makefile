.PHONY: demo build test fmt vet clean

## demo: run 3 backends + the balancer (STRATEGY=least-conn to switch)
demo:
	./scripts/demo.sh $(STRATEGY)

## build: compile the balancer and demo backend into ./bin
build:
	go build -o bin/lb ./cmd/lb
	go build -o bin/backend ./cmd/backend

## test: run all tests with the race detector
test:
	go test -race ./...

## fmt: format all Go code
fmt:
	gofmt -w .

## vet: run static analysis
vet:
	go vet ./...

## clean: remove build output
clean:
	rm -rf bin
