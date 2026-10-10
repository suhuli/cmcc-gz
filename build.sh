#!/usr/bin/env bash
# 交叉编译 Windows 版本（在 Linux / macOS 上运行）。
#   ./build.sh            # 版本号取自 git describe
#   VERSION=1.0.0 ./build.sh
set -euo pipefail
cd "$(dirname "$0")"

VERSION="${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
VERSION="${VERSION#v}"
LDFLAGS="-s -w -X main.version=${VERSION}"
export CGO_ENABLED=0

# 更新 exe 图标与版本信息（需要 go-winres；未安装时使用仓库中已有的 .syso）
if command -v go-winres >/dev/null 2>&1; then
  NUM="$(echo "$VERSION" | grep -oE '^[0-9]+(\.[0-9]+){0,3}' || true)"
  go run ./tools/genicon winres
  go-winres make --in winres/winres.json --out cmd/mcloudmount/rsrc --arch amd64,arm64 \
    ${NUM:+--product-version "$NUM" --file-version "$NUM"}
fi

go vet ./...
go test ./...

rm -rf dist && mkdir -p dist
for ARCH in amd64 arm64; do
  OUT="dist/mcloudmount-windows-${ARCH}"
  mkdir -p "$OUT"
  GOOS=windows GOARCH=$ARCH go build -trimpath -ldflags "${LDFLAGS} -H windowsgui" -o "$OUT/mcloudmount.exe" ./cmd/mcloudmount
  GOOS=windows GOARCH=$ARCH go build -trimpath -ldflags "${LDFLAGS}" -o "$OUT/mcloudmount-cli.exe" ./cmd/mcloudmount
  cp README.md LICENSE "$OUT/"
  (cd dist && zip -qr "mcloudmount-${VERSION}-windows-${ARCH}.zip" "mcloudmount-windows-${ARCH}")
done
(cd dist && sha256sum ./*.zip > SHA256SUMS.txt)
ls -lh dist
