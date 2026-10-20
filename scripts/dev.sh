#!/usr/bin/env bash
set -euo pipefail

# Gitea 개발 서버 실행 스크립트
# - 백엔드(Go)만 빌드한 뒤 gitea web 을 실행한다 (템플릿은 디스크에서 로드됨)
# - web_src/ 를 수정한 경우에만 `make frontend` 로 프론트엔드를 다시 빌드한다
# - 설정은 custom/conf/app.ini 를 사용한다 (없으면 개발용으로 생성)

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "${REPO_DIR}"

EXECUTABLE="gitea"
APP_INI="custom/conf/app.ini"
DATA_DIR="data"
TAGS="${TAGS:-sqlite}"
PORT="${PORT:-3000}"

log() { echo "[dev] $*"; }
err() { echo "[dev][ERROR] $*" >&2; }

if ! command -v go >/dev/null 2>&1; then
  err "go 명령어를 찾을 수 없습니다 (https://go.dev/dl/)"
  exit 1
fi

log "백엔드 빌드 중... (go build -tags ${TAGS})"
go build -tags "${TAGS}" -o "${EXECUTABLE}" .

mkdir -p "${DATA_DIR}" custom/conf

if [ ! -f "${APP_INI}" ]; then
  log "개발용 app.ini 생성 중 (포트 ${PORT})..."
  cat > "${APP_INI}" <<INI_EOF
[database]
DB_TYPE = sqlite3
PATH = data/gitea.db

[server]
APP_DATA_PATH = data/gitea
DOMAIN = localhost
HTTP_PORT = ${PORT}
ROOT_URL = http://localhost:${PORT}/

[repository]
ROOT = data/git/repositories

[security]
INSTALL_LOCK = false

[service]
REGISTER_EMAIL_CONFIRM = false
ENABLE_NOTIFY_MAIL = false
DISABLE_REGISTRATION = false
ENABLE_CAPTCHA = false

[log]
MODE = file
LEVEL = Debug

[i18n]
LANGS = en-US,ko-KR
INI_EOF
else
  log "app.ini 사용: ${APP_INI}"
fi

log "Gitea 개발 서버 시작 (중지: Ctrl+C)"
echo "---"

exec "./${EXECUTABLE}" web
