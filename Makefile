BINARY := git-cow
PKG    := ./cmd/git-cow

EXISTING_DIR := $(patsubst %/,%,$(dir $(shell command -v $(BINARY) 2>/dev/null)))
GOPATH_BIN   := $(shell go env GOPATH)/bin
LOCAL_BIN    := $(HOME)/.local/bin

ifeq ($(and $(EXISTING_DIR),$(shell test -w "$(EXISTING_DIR)" && echo yes)),yes)
  DEFAULT_BINDIR := $(EXISTING_DIR)
else ifeq ($(shell printf ':%s:' "$$PATH" | grep -q ":$(LOCAL_BIN):" && test -d "$(LOCAL_BIN)" && echo yes),yes)
  DEFAULT_BINDIR := $(LOCAL_BIN)
else
  DEFAULT_BINDIR := $(GOPATH_BIN)
endif

BINDIR ?= $(DEFAULT_BINDIR)

.PHONY: all build install uninstall test vet check bench fmt clean

all: build

build:
	go build -o $(BINARY) $(PKG)

install:
	@mkdir -p "$(BINDIR)"
	go build -o "$(BINDIR)/$(BINARY)" $(PKG)
	@echo "installed $(BINDIR)/$(BINARY)"
	@if command -v git >/dev/null 2>&1 && PATH="$(BINDIR):$$PATH" git cow help >/dev/null 2>&1; then \
		echo "git subcommand registered: 'git cow' is ready"; \
	fi
	@case ":$$PATH:" in \
		*":$(BINDIR):"*) ;; \
		*) echo "note: $(BINDIR) is not on PATH; add it so 'git cow' resolves" ;; \
	esac

uninstall:
	rm -f "$(BINDIR)/$(BINARY)"

test:
	go test -race ./...

vet:
	go vet ./...

check: vet test

bench:
	go test -run '^$$' -bench . ./cow

fmt:
	gofmt -s -w .

clean:
	rm -rf $(BINARY) bin/
