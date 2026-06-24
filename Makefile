.DEFAULT_GOAL := help

COMPOSE ?= docker compose

.PHONY: help config up down restart reload logs ps pull deploy update validate test build clean

help:
	@printf '%s\n' \
		'HubProxy ops targets:' \
		'  make config    Create local config.toml from config.example.toml if missing' \
		'  make up        Create config if needed and start the service' \
		'  make down      Stop and remove the compose service' \
		'  make restart   Restart the compose service' \
		'  make reload    Validate config and recreate service after config changes' \
		'  make logs      Follow service logs' \
		'  make ps        Show compose service status' \
		'  make pull      Pull the configured image' \
		'  make deploy    Pull image and recreate service' \
		'  make update    git pull --ff-only, then deploy' \
		'  make validate  Validate config.toml and compose config' \
		'  make test      Run Go tests' \
		'  make build     Build a local Docker image'

config:
	@if [ -f "config.toml" ]; then \
		echo "config.toml already exists"; \
	else \
		cp "config.example.toml" "config.toml"; \
		echo "Created config.toml from config.example.toml"; \
	fi

up: config
	$(COMPOSE) up -d

down:
	$(COMPOSE) down

restart:
	$(COMPOSE) restart hubproxy

reload: config validate
	$(COMPOSE) up -d --force-recreate hubproxy

logs:
	$(COMPOSE) logs -f hubproxy

ps:
	$(COMPOSE) ps

pull:
	$(COMPOSE) pull

deploy: config pull
	$(COMPOSE) up -d

update:
	git pull --ff-only
	$(MAKE) deploy

validate: config
	go run . -validate-config config.toml
	$(COMPOSE) config

test:
	go test ./...

build:
	docker build -t hubproxy:local-test .

clean:
	$(COMPOSE) down --remove-orphans
