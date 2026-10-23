.PHONY: test test-pg run lint

test:
	go test -race ./...

test-pg:
	TEST_DATABASE_URL=$${TEST_DATABASE_URL:-postgres://glucoguard:glucoguard@localhost:5432/glucoguard?sslmode=disable} go test ./internal/api/

run:
	go run ./cmd/server

lint:
	gofmt -l . && go vet ./...

.PHONY: web serve-web
web:
	GOOS=js GOARCH=wasm go build -trimpath -ldflags="-s -w" -o web/engine.wasm ./cmd/wasm
	cp "$$(go env GOROOT)/lib/wasm/wasm_exec.js" web/

serve-web: web
	cd web && python3 -m http.server 8000
