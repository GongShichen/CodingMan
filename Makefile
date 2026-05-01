APPDIR ?= $(HOME)/.codingman/app
PREFIX ?= /usr/local
BINDIR ?= $(PREFIX)/bin
BIN ?= CM
GO ?= go

.PHONY: build install uninstall test

build:
	mkdir -p bin
	$(GO) build -ldflags "-X main.defaultAppRoot=$(APPDIR)" -o bin/$(BIN) .

install: build
	mkdir -p "$(APPDIR)" "$(DESTDIR)$(BINDIR)" "$(HOME)/.codingman"
	rsync -a --delete \
		--exclude .git \
		--exclude .idea \
		--exclude .vscode \
		--exclude bin \
		--exclude dist \
		--exclude xsbin \
		--exclude .env \
		--exclude .codingman.log \
		--exclude '*.log' \
		./ "$(APPDIR)/"
	@if [ -f .env ] && [ ! -f "$(HOME)/.codingman/.env" ]; then \
		install -m 0600 .env "$(HOME)/.codingman/.env"; \
		echo "Initialized user config at $(HOME)/.codingman/.env"; \
	fi
	install -m 0755 "bin/$(BIN)" "$(DESTDIR)$(BINDIR)/$(BIN)"
	@echo "Installed $(BIN) to $(DESTDIR)$(BINDIR)/$(BIN)"
	@echo "Installed CodingMan app files to $(APPDIR)"

uninstall:
	rm -f "$(DESTDIR)$(BINDIR)/$(BIN)"
	rm -rf "$(APPDIR)"
	rm -rf "$(HOME)/.codingman"
	@echo "Removed $(DESTDIR)$(BINDIR)/$(BIN)"
	@echo "Removed CodingMan app files from $(APPDIR)"
	@echo "Removed all CodingMan user data from $(HOME)/.codingman"

test:
	$(GO) test ./...
	(cd agent && $(GO) test ./...)
	(cd tool && $(GO) test ./...)
