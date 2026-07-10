BINARY   := juice-agent
PKG      := github.com/daios-ai/juice-agent/cmd/juice-agent
PREFIX   ?= $(HOME)/.local
JUICE_BIN ?= ../juice/juice
VERSION  := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS  := -ldflags "-X main.version=$(VERSION) -X main.commit=$(COMMIT)"

.PHONY: build test flows bench clean install uninstall

build:
	go build $(LDFLAGS) -o $(BINARY) $(PKG)

test:
	go test ./... -count=1

# End-to-end flows against the compiled juice-agent + a real juice kernel + real
# Ollama (the LLM the kernel ships with). Mechanism flows run offline; LLM flows
# skip if Ollama/the chat model is unavailable. Override the models with
# OLLAMA_CHAT_MODEL / OLLAMA_EMBED_MODEL.
flows: build
	JUICE_BIN=$(JUICE_BIN) ./flows/flows_test.sh

# Experiment suite against a live kernel + real LLM (not part of `go test`).
# Requires a reachable kernel and AGENT_PASSWORD.
bench: build
	./$(BINARY) bench bench/tasks.json

clean:
	rm -f $(BINARY)

install: build
	mkdir -p $(PREFIX)/bin
	install -m 755 $(BINARY) $(PREFIX)/bin/$(BINARY)

# Removes the installed binary only; agent data (transcripts, task index) is left intact.
uninstall:
	rm -f $(PREFIX)/bin/$(BINARY)
