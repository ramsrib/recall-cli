BINARY  := recall
BINDIR  ?= $(HOME)/.local/bin
GO      ?= go

.DEFAULT_GOAL := help

.PHONY: help build install uninstall clean fmt vet tidy

help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

build: ## Build the binary into the repo dir
	$(GO) build -o $(BINARY) .

install: build ## Build and install to ~/.local/bin (override with BINDIR=...)
	@mkdir -p "$(BINDIR)"
	@install -m 0755 "$(BINARY)" "$(BINDIR)/$(BINARY)"
	@echo "installed → $(BINDIR)/$(BINARY)"
	@case ":$$PATH:" in \
		*":$(BINDIR):"*) echo "✓ $(BINDIR) is on your PATH — try: recall index" ;; \
		*) echo "⚠ $(BINDIR) is NOT on your PATH. Add it to your shell rc, e.g.:"; \
		   echo "    echo 'export PATH=\"$(BINDIR):\$$PATH\"' >> ~/.zshrc && exec zsh" ;; \
	esac

uninstall: ## Remove the installed binary
	@rm -f "$(BINDIR)/$(BINARY)" && echo "removed → $(BINDIR)/$(BINARY)"

clean: ## Remove the locally built binary
	@rm -f "$(BINARY)"

fmt: ## gofmt the tree
	$(GO) fmt ./...

vet: ## go vet the tree
	$(GO) vet ./...

tidy: ## Tidy go.mod / go.sum
	$(GO) mod tidy
