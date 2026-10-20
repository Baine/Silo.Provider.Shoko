BINARY := silo-plugin-shoko

.PHONY: build test vet fmt check dist clean

build:
	go build -trimpath -o $(BINARY) .

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

check: vet test

# Cross-compiled release binaries, named with the version from manifest.json.
dist:
	@mkdir -p dist
	@version=$$(sed -n 's/.*"version": "\([^"]*\)".*/\1/p' manifest.json); \
	for target in linux/amd64 linux/arm64 darwin/arm64; do \
		os=$${target%/*}; arch=$${target#*/}; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -trimpath \
			-o dist/$(BINARY)_$${version}_$${os}_$${arch} . || exit 1; \
	done

clean:
	rm -f $(BINARY)
	rm -rf dist
