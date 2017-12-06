VERSION=$(shell git rev-parse --short HEAD)

all: build

build:
	docker build -t goodeggs/convox-agent .

test:
	go test -cover -v ./...

vendor:
	godep save -r -copy=true ./...

release: build
	docker tag goodeggs/convox-agent:latest goodeggs/convox-agent:$(VERSION)
	docker push goodeggs/convox-agent:$(VERSION)
	#AWS_DEFAULT_PROFILE=release aws s3 cp convox.conf s3://convox/agent/0.73/convox.conf --acl public-read
