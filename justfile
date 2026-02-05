binary := "csm"
build_dir := "./build"
install_dir := "~/.local/bin"
ldflags := "-s -w"

# List available recipes
_default:
    @just --list

# Build the binary
build:
    @echo "Building {{binary}}..."
    @mkdir -p {{build_dir}}
    @go build -ldflags "{{ldflags}}" -o {{build_dir}}/{{binary}} .

# Build and install to ~/.local/bin
install: build
    @echo "Installing {{binary}} to {{install_dir}}..."
    @mkdir -p {{install_dir}}
    @cp {{build_dir}}/{{binary}} {{install_dir}}/{{binary}}
    @echo "Installed! Make sure {{install_dir}} is in your PATH"

# Remove build artifacts
clean:
    @echo "Cleaning..."
    @rm -rf {{build_dir}}

# Build and run
run: build
    @{{build_dir}}/{{binary}}

# Run tests
test:
    @go test ./...

# Format code
fmt:
    @go fmt ./...

# Lint code (requires golangci-lint)
lint:
    @golangci-lint run
