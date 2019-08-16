IMAGE := mediocregopher/buckaroo-banzai
GITREF := $(shell git rev-parse HEAD)

binary:
	go build ./cmd/buckaroo-banzai

docker-binary:
	CGO_ENABLED=0 go build -ldflags "-X main.gitRef=${GITREF}" -a -installsuffix cgo ./cmd/buckaroo-banzai

docker: docker-binary
	docker build -t $(IMAGE):$(GITREF) -t $(IMAGE):latest .

docker-push:
	docker push $(IMAGE):$(GITREF)
	docker push $(IMAGE):latest
