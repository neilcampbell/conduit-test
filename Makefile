.PHONY: conduit test fmt docker release

LDFLAGS += -X github.com/algorand/conduit/version.Hash=$(shell git log -n 1 --pretty="%H")
LDFLAGS += -X github.com/algorand/conduit/version.ShortHash=$(shell git log -n 1 --pretty="%h")
LDFLAGS += -X github.com/algorand/conduit/version.CompileTime=$(shell date -u +%Y-%m-%dT%H:%M:%S%z)
LDFLAGS += -X "github.com/algorand/conduit/version.ReleaseVersion=Custom Plugin Build"

# Docker image configuration
IMAGE_NAME ?= neilcampbell/conduit-localnet
IMAGE_TAG ?= latest
ARCH ?= amd64


- name: Build and push
        uses: docker/build-push-action@v6
        with:
          context: ./resources/mock-server
          push: true
          tags: |
            ${{ env.IMAGE }}:latest
            ${{ env.IMAGE }}:${{ github.sha }}
          cache-from: type=gha
          cache-to: type=gha,mode=max
          platforms: linux/amd64,linux/arm64
conduit:
	go build -ldflags='${LDFLAGS}' -o conduit cmd/conduit/main.go
	./conduit -v

test:
	go test ./...

fmt:
	go fmt ./...

# Build for specified architecture (default: amd64)
# Examples:
#   make docker                    # builds for amd64
#   make docker ARCH=arm64         # builds for arm64
docker:
	docker build --build-arg TARGETARCH=${ARCH} -t ${IMAGE_NAME}:${IMAGE_TAG} .

release:
	@echo "\nConfiguring .goreleaser"
	build/sync-config.sh
	@echo "Build everything with:"
	@echo "   goreleaser release --skip-publish --snapshot --clean"
