BINARY  := podcaptain
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
PREFIX  ?= $(HOME)/.local
CONFIG  ?= $(HOME)/.config/podcaptain/config.yaml

.PHONY: build test vet install config install-launchd uninstall-launchd install-systemd uninstall-systemd clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/podcaptain

test:
	go test -race ./...

vet:
	go vet ./...

install: build
	install -d $(PREFIX)/bin
	install -m 0755 bin/$(BINARY) $(PREFIX)/bin/$(BINARY)

# Copies the example config if none exists yet.
config:
	@install -d $(dir $(CONFIG))
	@if [ -e $(CONFIG) ]; then echo "$(CONFIG) exists, leaving it alone"; \
	else install -m 0600 config.example.yaml $(CONFIG) && echo "wrote $(CONFIG) - edit it before starting the service"; fi

LAUNCHD_PLIST := $(HOME)/Library/LaunchAgents/local.podcaptain.plist

install-launchd: install config
	install -d $(HOME)/Library/Logs
	sed -e 's|@BINARY@|$(PREFIX)/bin/$(BINARY)|' -e 's|@CONFIG@|$(CONFIG)|' -e 's|@LOGDIR@|$(HOME)/Library/Logs|' \
		deploy/launchd/local.podcaptain.plist > $(LAUNCHD_PLIST)
	-launchctl bootout gui/$$(id -u) $(LAUNCHD_PLIST) 2>/dev/null
	launchctl bootstrap gui/$$(id -u) $(LAUNCHD_PLIST)
	@echo "podcaptain loaded; logs: $(HOME)/Library/Logs/podcaptain.log"

uninstall-launchd:
	-launchctl bootout gui/$$(id -u) $(LAUNCHD_PLIST)
	rm -f $(LAUNCHD_PLIST)

install-systemd: install config
	install -d $(HOME)/.config/systemd/user
	install -m 0644 deploy/systemd/podcaptain.service $(HOME)/.config/systemd/user/podcaptain.service
	systemctl --user daemon-reload
	systemctl --user enable --now podcaptain
	@echo "podcaptain enabled; logs: journalctl --user -u podcaptain -f"

uninstall-systemd:
	-systemctl --user disable --now podcaptain
	rm -f $(HOME)/.config/systemd/user/podcaptain.service
	systemctl --user daemon-reload

clean:
	rm -rf bin
