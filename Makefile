BINARY=samplellama
GO=go
LDFLAGS=-ldflags="-w -s"
BUILD_ENV=CGO_ENABLED=0

.PHONY: all build clean lint-docs test

all: build

build: $(BINARY)

$(BINARY): $(wildcard *.go)
	$(BUILD_ENV) $(GO) build $(LDFLAGS) -o $(BINARY)

clean:
	rm -f $(BINARY)

lint-docs:
	@echo "Linting manpage..."
	@mandoc -Tlint samplellama.1
	@echo "Linting markdown documentation..."
	@markdownlint-cli2 README.md DESIGN.md

test:
	$(GO) test ./...
