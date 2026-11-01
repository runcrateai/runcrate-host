VERSION ?= dev
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: test check-models release clean

test:
	go test -race ./...
	sh hack/test-install.sh

# install.sh's list of the GPU models Runcrate sells against the web app's
# (APP: a checkout of Runcrate's web app).
check-models:
	@test -n "$(APP)" || { echo "usage: make check-models APP=<Runcrate web app checkout>"; exit 2; }
	sh hack/check-models.sh "$(APP)/src/lib/providers/gpu-names.ts"

release:
	mkdir -p out
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o out/runcrate-host-linux-amd64 ./cmd/runcrate-host
	cd out && sha256sum runcrate-host-linux-amd64 > runcrate-host-linux-amd64.sha256 2>/dev/null || shasum -a 256 runcrate-host-linux-amd64 > runcrate-host-linux-amd64.sha256
	echo $(VERSION) > out/runcrate-host-linux-amd64.version
	cp install.sh out/install.sh

clean:
	rm -rf out
