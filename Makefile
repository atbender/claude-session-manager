.PHONY: build install clean run test

# Binary name
BINARY=csm

# Build directory
BUILD_DIR=./build

# Install directory
INSTALL_DIR=~/.local/bin

# Go build flags
LDFLAGS=-s -w

build:
	@echo "Building $(BINARY)..."
	@mkdir -p $(BUILD_DIR)
	@go build -ldflags "$(LDFLAGS)" -o $(BUILD_DIR)/$(BINARY) .

install: build
	@echo "Installing $(BINARY) to $(INSTALL_DIR)..."
	@mkdir -p $(INSTALL_DIR)
	@cp $(BUILD_DIR)/$(BINARY) $(INSTALL_DIR)/$(BINARY)
	@echo "Installed! Make sure $(INSTALL_DIR) is in your PATH"

clean:
	@echo "Cleaning..."
	@rm -rf $(BUILD_DIR)

run: build
	@$(BUILD_DIR)/$(BINARY)

test:
	@go test ./...

# Development: build and run
dev: build
	@$(BUILD_DIR)/$(BINARY)

# Format code
fmt:
	@go fmt ./...

# Lint code (requires golangci-lint)
lint:
	@golangci-lint run

# Show help
help:
	@echo "Available targets:"
	@echo "  build   - Build the binary"
	@echo "  install - Build and install to ~/.local/bin"
	@echo "  clean   - Remove build artifacts"
	@echo "  run     - Build and run"
	@echo "  test    - Run tests"
	@echo "  dev     - Build and run (alias for run)"
	@echo "  fmt     - Format code"
	@echo "  lint    - Lint code"
	@echo "  help    - Show this help"
