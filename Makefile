.PHONY: build run clean test install

BINARY_NAME=ncc
INSTALL_PATH=/usr/local/bin

build:
	go build -ldflags="-s -w" -o $(BINARY_NAME) ./cmd/cli

run: build
	./$(BINARY_NAME)

clean:
	rm -f $(BINARY_NAME)
	go clean

test:
	go test ./...

install: build
	cp $(BINARY_NAME) $(INSTALL_PATH)/$(BINARY_NAME)
	chmod +x $(INSTALL_PATH)/$(BINARY_NAME)

deps:
	go mod download
	go mod tidy

example-encode: build
	./$(BINARY_NAME) -mode=encode -input=README.md -output=readme_ncc.mp4

example-decode: build
	./$(BINARY_NAME) -mode=decode -input=readme_ncc.mp4 -output=README_recovered.md
