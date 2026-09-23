.PHONY: build vpcctl test test-race test-integration check docs release-artifacts
build:
	go build -trimpath -o bin/global-vpc-controller ./cmd/global-vpc-controller
	go build -trimpath -o bin/site-vpc-controller ./cmd/site-vpc-controller
	go build -trimpath -o bin/platform-vpc-controller ./cmd/platform-vpc-controller
	go build -trimpath -o bin/vpcctl ./cmd/vpcctl

vpcctl:
	go build -trimpath -o bin/vpcctl ./cmd/vpcctl

test:
	go test ./...
	python3 -m unittest discover -s tests -v

test-race:
	go test -race ./...

test-integration:
	test -n "$(KUBEBUILDER_ASSETS)"
	go test -tags=integration -v ./internal/controller -run TestAPIServerLifecycle -count=1
	go test -tags=integration -v ./internal/sitecontroller -count=1
	go test -tags=integration -v ./api/v1alpha2 ./internal/platformcontroller ./internal/localcontroller -count=1
	go test -tags=integration -v ./internal/managedgateway -count=1
	go test -tags=integration -v ./internal/vpcctl -count=1

check:
	go vet ./...

docs:
	python3 scripts/build-docs.py --output public --check

release-artifacts:
	python3 scripts/package-release.py
