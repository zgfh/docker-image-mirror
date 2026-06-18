.PHONY: build run docker-build docker-run docker-stop

build:
	go build -o bin/server ./cmd/server

run: build
	./bin/server

docker-build:
	docker build -t docker-image-mirror:latest .

docker-run:
	docker-compose up -d

docker-stop:
	docker-compose down
