# usage
# eg. make release VERSION=v0.1.5
# Binary name
BINARY=gomysql2pg
# Builds the project
build:
		GO111MODULE=on go build -o ${BINARY} -ldflags "-X main.Version=${VERSION}"
		GO111MODULE=on go test -v
# Installs our project: copies binaries
install:
		GO111MODULE=on go install
release:
		# Clean
		go clean
		rm -rf *.gz
		# Build for mac
		GO111MODULE=on GOOS=darwin go build -ldflags "-s -w -X main.Version=${VERSION}"
		GO111MODULE=on GOOS=darwin go build -o xlsx2yml -ldflags "-s -w" ./tools/xlsx2yml
		COPYFILE_DISABLE=1 tar czvf ${BINARY}-MacOS-x64-${VERSION}.tar.gz ./${BINARY} ./xlsx2yml ./example.yml ./check_log.ps1 ./check_log.sh ./run_batch.ps1 ./run_batch.sh ./configs/example.xlsx
		# Build for arm
		go clean
		rm -f xlsx2yml
		CGO_ENABLED=0 GOOS=linux GOARCH=arm64 GO111MODULE=on go build -ldflags "-s -w -X main.Version=${VERSION}"
		CGO_ENABLED=0 GOOS=linux GOARCH=arm64 GO111MODULE=on go build -o xlsx2yml -ldflags "-s -w" ./tools/xlsx2yml
		COPYFILE_DISABLE=1 tar czvf ${BINARY}-linux-arm64-${VERSION}.tar.gz ./${BINARY} ./xlsx2yml ./example.yml ./check_log.ps1 ./check_log.sh ./run_batch.ps1 ./run_batch.sh ./configs/example.xlsx
		# Build for linux
		go clean
		rm -f xlsx2yml
		CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GO111MODULE=on go build -ldflags "-s -w -X main.Version=${VERSION}"
		CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GO111MODULE=on go build -o xlsx2yml -ldflags "-s -w" ./tools/xlsx2yml
		COPYFILE_DISABLE=1 tar czvf ${BINARY}-linux-x64-${VERSION}.tar.gz ./${BINARY} ./xlsx2yml ./example.yml ./check_log.ps1 ./check_log.sh ./run_batch.ps1 ./run_batch.sh ./configs/example.xlsx
		# Build for win
		go clean
		rm -f xlsx2yml
		CGO_ENABLED=0 GOOS=windows GOARCH=amd64 GO111MODULE=on go build -ldflags "-s -w -X main.Version=${VERSION}"
		CGO_ENABLED=0 GOOS=windows GOARCH=amd64 GO111MODULE=on go build -o xlsx2yml.exe -ldflags "-s -w" ./tools/xlsx2yml
		COPYFILE_DISABLE=1 tar czvf ${BINARY}-win-x64-${VERSION}.tar.gz ./${BINARY}.exe ./xlsx2yml.exe ./example.yml ./check_log.ps1 ./check_log.sh ./run_batch.ps1 ./run_batch.sh ./configs/example.xlsx
		go clean
		rm -f xlsx2yml xlsx2yml.exe
# Cleans our projects: deletes binaries
clean:
		go clean
		rm -rf *.gz

.PHONY:  clean build