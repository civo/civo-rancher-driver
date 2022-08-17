.PHONY: build clean

build:
	mkdir -p ./bin
	@echo "Building binary for linux-amd64"
	export GOOS=linux GOARCH=amd64
	go build -o bin/civo-rancher-driver-linux-amd64
	@echo "Built linux-amd64"

clean:
	rm -rf ./bin
