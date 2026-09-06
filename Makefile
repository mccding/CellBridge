.PHONY: test probe build admin diagnostics backup turn-test ios-project ios-test install module-agent-test module-agent-build verify

test:
	cd gateway && go test ./...

build:
	cd gateway && go build ./...

probe:
	cd gateway && go run ./cmd/cellbridge-probe --json

admin:
	cd gateway && go build ./cmd/cellbridge-admin

diagnostics:
	cd gateway && go run ./cmd/cellbridge-admin diagnostics export --output ../cellbridge-diagnostics.zip

backup:
	cd gateway && go run ./cmd/cellbridge-admin backup --data-dir /var/lib/cellbridge --output ../cellbridge-backup.zip

turn-test:
	cd gateway && go run ./cmd/cellbridge-admin turn-test

install:
	./infra/install.sh

module-agent-test:
	cd module-agent && go test ./...

module-agent-build:
	mkdir -p artifacts/module-agent
	cd module-agent && GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o ../artifacts/module-agent/cellbridge-agent-armv7 ./cmd/cellbridge-agent

ios-project:
	xcodegen generate --spec ios/project.yml --project ios

ios-test: ios-project
	xcodebuild test -project ios/CellBridge.xcodeproj -scheme CellBridge -destination 'platform=iOS Simulator,name=iPhone 17 Pro,OS=latest' CODE_SIGNING_ALLOWED=NO

verify: test build ios-test
