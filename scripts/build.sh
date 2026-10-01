#!/usr/bin/env bash
set -euo pipefail

PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
APP_VERSION="${IPM_VERSION:-$(git -C "$PROJECT_ROOT" describe --tags --exact-match 2>/dev/null || true)}"
APP_COMMIT="$(git -C "$PROJECT_ROOT" rev-parse HEAD 2>/dev/null || echo unknown)"
APP_BUILT_AT="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

if [[ -z "$APP_VERSION" ]]; then
  APP_VERSION="2.2.4"
fi

BUILD_LDFLAGS="-s -w -X icloud-privacy-mail-v2/internal/buildinfo.Version=$APP_VERSION -X icloud-privacy-mail-v2/internal/buildinfo.Commit=$APP_COMMIT -X icloud-privacy-mail-v2/internal/buildinfo.BuiltAt=$APP_BUILT_AT"

echo "正在构建 Vue 前端……"
(
  cd "$PROJECT_ROOT/frontend"
  npm ci
  npm run build
)

"$PROJECT_ROOT/scripts/sync-web.sh"

echo "正在构建 Go 服务……"
mkdir -p "$PROJECT_ROOT/bin"
(

  cd "$PROJECT_ROOT"
  go build -trimpath -ldflags "$BUILD_LDFLAGS" -o "$PROJECT_ROOT/bin/ipm-server" .
)

echo "构建完成，版本 ${APP_VERSION}，提交 ${APP_COMMIT:0:7}，服务程序位于 bin/ipm-server。"
