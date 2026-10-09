BINARY   := juice-agent
PKG      := github.com/daios-ai/juice-agent/cmd/juice-agent
JUICE_HOME ?= $(HOME)/.juice
JUICE_SRC  ?= ../juice
JUICE_BIN  ?= $(JUICE_SRC)/juice
VERSION  := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS  := -ldflags "-X main.version=$(VERSION) -X main.commit=$(COMMIT)"

.PHONY: build test flows bench clean install uninstall

build:
	go build $(LDFLAGS) -o $(BINARY) $(PKG)

test:
	go test ./... -count=1

# End-to-end flows against the compiled juice-agent + a real juice kernel + real
# Ollama (the LLM the kernel ships with). The kernel checkout (JUICE_SRC) supplies
# the kernel binary and its flow harness. Mechanism flows need no model; LLM flows
# skip if Ollama/the chat model is unavailable.
flows: build
	JUICE_SRC=$(JUICE_SRC) JUICE_BIN=$(JUICE_BIN) ./flows/flows_test.sh

# Experiment suite against a live kernel + real LLM (not part of `go test`).
# Requires LOGIN=handle@kernel with a session (juice auth login).
bench: build
	./$(BINARY) bench $(LOGIN) bench/tasks.json

clean:
	rm -f $(BINARY)

# The juice family's executables live in $JUICE_HOME/bin (ecosystem standard §2).
install: build
	mkdir -p $(JUICE_HOME)/bin
	install -m 755 $(BINARY) $(JUICE_HOME)/bin/$(BINARY)

# Removes the installed binary only; agent homes are left intact.
uninstall:
	rm -f $(JUICE_HOME)/bin/$(BINARY)
