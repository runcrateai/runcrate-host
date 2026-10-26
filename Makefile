VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: test release clean

test:
	go test -race ./...

release:
	mkdir -p out
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o out/runcrate-host-linux-amd64 ./cmd/runcrate-host
	cd out && sha256sum runcrate-host-linux-amd64 > runcrate-host-linux-amd64.sha256 2>/dev/null || shasum -a 256 runcrate-host-linux-amd64 > runcrate-host-linux-amd64.sha256
	cp install.sh out/install.sh

clean:
	rm -rf out
