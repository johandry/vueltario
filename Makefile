.PHONY: build run worker test migrate clean

BINARY := bin/vueltario

build:
	@mkdir -p bin
	go build -o $(BINARY) ./cmd/vueltario

run: build
	./$(BINARY) serve --with-worker

worker: build
	./$(BINARY) worker

migrate: build
	./$(BINARY) migrate up

test:
	go test -race -count=1 ./...

clean:
	rm -rf bin coverage.txt coverage.html
