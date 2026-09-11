BIN := bin
PKGS := ./...
LDFLAGS := -X github.com/kyosu-1/tetherd/internal/version.Version=$(shell git describe --tags --always --dirty)

ECR_REGISTRY ?=
TAG ?= dev

.PHONY: build test lint clean e2e-local push-images

build:
	mkdir -p $(BIN)
	go build -ldflags '$(LDFLAGS)' -o $(BIN)/tetherd ./cmd/tetherd
	go build -ldflags '$(LDFLAGS)' -o $(BIN)/tetherd-agent ./cmd/tetherd-agent
	go build -ldflags '$(LDFLAGS)' -o $(BIN)/tetherd-helper ./cmd/tetherd-helper
	go build -ldflags '$(LDFLAGS)' -o $(BIN)/tetherd-exec ./cmd/tetherd-exec

test:
	go test -race -count=1 $(PKGS)

lint:
	go vet $(PKGS)

clean:
	rm -rf $(BIN)

e2e-local: build
	bash hack/e2e-local.sh

# Build linux/arm64 images (Fargate runs Graviton in dev-env) and push to ECR.
# Usage: aws ecr get-login-password --profile personal | docker login --username AWS --password-stdin $ECR_REGISTRY
#        make push-images ECR_REGISTRY=738925651667.dkr.ecr.ap-northeast-1.amazonaws.com
push-images:
	@test -n "$(ECR_REGISTRY)" || { echo "ECR_REGISTRY is required"; exit 2; }
	docker buildx build --platform linux/arm64 -f deploy/docker/agent.Dockerfile -t $(ECR_REGISTRY)/tetherd-agent:$(TAG) --push .
	docker buildx build --platform linux/arm64 -f deploy/docker/sampleapp.Dockerfile -t $(ECR_REGISTRY)/tetherd-sampleapp:$(TAG) --push .
