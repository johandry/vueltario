.PHONY: build build-desktop run worker test migrate clean

BINARY := bin/vueltario
DESKTOP_BINARY := bin/vueltario-desktop

build:
	@mkdir -p bin
	go build -o $(BINARY) ./cmd/vueltario

build-desktop:
	@mkdir -p bin
	go build -o $(DESKTOP_BINARY) ./cmd/vueltario-desktop

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
