IMAGE := docker.mediocre-desktop.zt/buckaroo-banzai
GITREF := $(shell git rev-parse HEAD)

build: docker-binary
	docker build -t $(IMAGE):$(GITREF) -t $(IMAGE):latest .

docker-binary:
	CGO_ENABLED=0 GOOS=linux go build -ldflags "-X main.gitRef=${GITREF}" -a -installsuffix cgo .

push:
	docker push $(IMAGE):$(GITREF)
	docker push $(IMAGE):latest

restart:
	docker-compose down
	docker-compose up -d
