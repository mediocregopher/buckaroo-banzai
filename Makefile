IMAGE := docker.mediocre-desktop.zt/buckaroo-banzai
GITREF := $(shell git rev-parse HEAD)

build:
	docker build -t $(IMAGE):$(GITREF) -t $(IMAGE):latest .

push:
	docker push $(IMAGE):$(GITREF)
	docker push $(IMAGE):latest
