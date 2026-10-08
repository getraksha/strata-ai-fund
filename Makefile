# Strata: common tasks. `make` lists them.
# Targets marked (model) call OpenAI: set OPENAI_API_KEY and OPENAI_MODEL first.

BIN        ?= strata
PROJECT    ?= cli-test
UI_PROJECT ?= ui-test
ADDR       ?= 127.0.0.1:8080
RUNS       ?= 3

.DEFAULT_GOAL := help
.PHONY: help build test vet fmt check serve walkthrough eval clean

help: ## list targets
	@grep -E '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-12s %s\n", $$1, $$2}'

build: ## build ./strata
	go build -o $(BIN) ./cmd/strata

test: ## run all tests (offline, scripted model, no API key)
	go test ./...

vet: ## go vet
	go vet ./...

fmt: ## gofmt every file
	gofmt -w .

check: vet test ## vet + test, and fail if any file needs gofmt
	@test -z "$$(gofmt -l .)" || (echo "needs gofmt:"; gofmt -l .; exit 1)

serve: build ## (model) web UI: make serve [UI_PROJECT=ui-test] [ADDR=127.0.0.1:8080]
	./$(BIN) serve -project $(UI_PROJECT) -addr $(ADDR)

walkthrough: build ## (model) README CLI walkthrough on a new project: make walkthrough [PROJECT=cli-test]
	@test ! -e $(PROJECT) || (echo "$(PROJECT)/ already exists: run 'make clean' or pass PROJECT=<new name>"; exit 1)
	./$(BIN) init     -project $(PROJECT)
	./$(BIN) ingest   -project $(PROJECT) testdata/v1_proposed.md
	./$(BIN) ingest   -project $(PROJECT) testdata/v2_revised.md
	./$(BIN) ingest   -project $(PROJECT) testdata/v3_final.md
	./$(BIN) cards    -project $(PROJECT)
	@echo "--- expected to be refused: C4 is not routed to Lena (P-03)"
	-./$(BIN) review  -project $(PROJECT) -as P-03 -approve C4
	./$(BIN) review   -project $(PROJECT) -as P-02 -approve C4
	./$(BIN) state    -project $(PROJECT)
	./$(BIN) rollback -project $(PROJECT) -as P-01 -to 16 -note "walkthrough: undo C4 approval"
	./$(BIN) state    -project $(PROJECT)
	./$(BIN) log      -project $(PROJECT)

eval: build ## (model) scorecard against testdata/expected_changes.json: make eval [RUNS=3]
	./$(BIN) eval -runs $(RUNS)

clean: ## remove the binary and local test projects (keeps demo/)
	rm -rf $(BIN) .strata $(PROJECT) $(UI_PROJECT)
