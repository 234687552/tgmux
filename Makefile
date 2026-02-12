.PHONY: build dev test lint clean

build:
	go build -o bin/tgmux .

dev:
	go run -race .

test:
	go test ./...

lint:
	golangci-lint run

clean:
	rm -rf bin/
