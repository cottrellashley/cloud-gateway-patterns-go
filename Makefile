.PHONY: check test vet fmt run

check: fmt vet test

fmt:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	go vet ./...

test:
	go test -race -count=1 ./...

run:
	go run ./cmd/gateway -upstream http://127.0.0.1:9000
