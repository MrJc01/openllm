.PHONY: all build clean test

all: build

build:
	@echo "Building openllm CLI..."
	go build -o openllm cmd/openllm/main.go
	@echo "Building openllmd daemon..."
	go build -o openllmd cmd/openllmd/main.go
	@echo "Building openllm-watchdog remote helper..."
	go build -o openllm-watchdog cmd/openllm-watchdog/main.go
	@echo "Done! All binaries compiled successfully."

clean:
	@echo "Cleaning up binaries..."
	rm -f openllm openllmd openllm-watchdog
	@echo "Clean complete."

test:
	@echo "Running tests..."
	go test -v ./...
