# Top-level build: the C++ core library first, then the Go binary that links it.

BUILD      ?= build
CORE_BUILD := $(BUILD)/core
SAN_BUILD  := $(BUILD)/core-san
JOBS       ?= $(shell getconf _NPROCESSORS_ONLN 2>/dev/null || echo 4)
GO         ?= go
CMAKE      ?= cmake

.PHONY: all core core-test go-build go-test lint test sanitize go-asan riscv-tests compliance difftest archtest-elfs archtest platform bench toolchain clean

all: go-build

## core: configure and build librvcore.a (Release) plus its unit tests
core:
	$(CMAKE) -S core -B $(CORE_BUILD) -DCMAKE_BUILD_TYPE=Release -DRVSIM_SANITIZE=OFF -DRVSIM_BUILD_TESTS=ON
	$(CMAKE) --build $(CORE_BUILD) -j $(JOBS)

core-test: core
	ctest --test-dir $(CORE_BUILD) --output-on-failure

## go-build: build bin/rvsim (cgo links $(CORE_BUILD)/librvcore.a)
go-build: core
	$(GO) build -o bin/rvsim ./cmd/rvsim

go-test: core
	$(GO) test ./...

lint: core
	$(GO) vet ./...
	$(GO) tool staticcheck ./...

test: core-test go-test lint

## sanitize: C++ unit tests under ASan + UBSan (Debug)
sanitize:
	$(CMAKE) -S core -B $(SAN_BUILD) -DCMAKE_BUILD_TYPE=Debug -DRVSIM_SANITIZE=ON
	$(CMAKE) --build $(SAN_BUILD) -j $(JOBS)
	ctest --test-dir $(SAN_BUILD) --output-on-failure

## go-asan: Go tests against an ASan/UBSan core (Linux only: go -asan is unsupported on macOS).
## Rebuilds $(CORE_BUILD) sanitized; the next `make core` switches it back.
go-asan:
	$(CMAKE) -S core -B $(CORE_BUILD) -DCMAKE_BUILD_TYPE=Debug -DRVSIM_SANITIZE=ON -DRVSIM_BUILD_TESTS=OFF
	$(CMAKE) --build $(CORE_BUILD) -j $(JOBS)
	CGO_LDFLAGS=-fsanitize=undefined $(GO) test -asan -count=1 ./...

## riscv-tests: fetch (pinned) and build the riscv-tests suites into $(BUILD)/riscv-tests
riscv-tests:
	tests/compliance/build-riscv-tests.sh $(BUILD)/riscv-tests

## compliance: run the riscv-tests suites on rvsim
compliance: core riscv-tests
	$(GO) test -count=1 ./tests/compliance

## difftest: compare commit logs against the pinned Spike, report the first divergence
difftest: core riscv-tests
	$(GO) test -count=1 ./tests/difftest

## archtest-elfs: generate ACT4 (riscv-arch-test) ELFs with Sail for rvsim's config
archtest-elfs:
	tests/archtest/build-archtest.sh $(abspath $(BUILD))/archtest

## archtest: run the generated ACT4 ELFs on rvsim
archtest: core
	$(GO) test -count=1 ./tests/archtest

## platform: bare-metal programs exercising the devices (needs riscv64-unknown-elf-gcc)
platform: core
	$(GO) test -count=1 ./tests/platform

bench: core
	$(GO) test -run '^$$' -bench . -benchmem ./...

toolchain:
	scripts/toolchain.sh

clean:
	rm -rf $(BUILD) bin
