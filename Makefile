.PHONY: build cross-build test
VERSION ?= 0.2.0
LDFLAGS = -s -w -X main.version=$(VERSION)

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/scanspool ./cmd/scanspool

cross-build:
	@set -eu; for target_os in linux darwin windows; do \
	  for target_arch in amd64 arm64; do \
	    suffix=""; if [ "$$target_os" = windows ]; then suffix=.exe; fi; \
	    echo "Building $$target_os/$$target_arch"; \
	    CGO_ENABLED=0 GOOS=$$target_os GOARCH=$$target_arch go build -trimpath -ldflags "$(LDFLAGS)" -o "bin/scanspool-$$target_os-$$target_arch$$suffix" ./cmd/scanspool; \
	  done; \
	done

test:
	CGO_ENABLED=0 go test -timeout 60s ./...
