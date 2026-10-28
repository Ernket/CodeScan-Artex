#!/usr/bin/env bash
# SCA Audit 安装脚本：本地编译运行（需要已就绪的 PostgreSQL）
set -euo pipefail
cd "$(cd "$(dirname "$0")" && pwd)"

info(){ printf '\033[36m[*]\033[0m %s\n' "$*"; }
ok(){   printf '\033[32m[+]\033[0m %s\n' "$*"; }
warn(){ printf '\033[33m[!]\033[0m %s\n' "$*"; }
die(){  printf '\033[31m[x]\033[0m %s\n' "$*" >&2; exit 1; }
ask(){  local p="$1" d="${2:-}" a; read -rp "$p${d:+ [$d]}: " a; echo "${a:-$d}"; }

echo "=============================="
echo "  SCA Audit 安装（本地编译）"
echo "=============================="

info "请填写已就绪的 PostgreSQL 连接信息："
DB_HOST="$(ask '数据库地址' 127.0.0.1)"
DB_PORT="$(ask '端口' 5432)"
DB_USER="$(ask '账号' artex)"
DB_PASS="$(ask '密码' '')"
DB_NAME="$(ask '数据库名' artex)"
DB_SSL="$(ask 'sslmode (disable/require)' disable)"

# 生成 config.json
cat > config.json <<JSON
{
  "database": {
    "host": "${DB_HOST}",
    "port": ${DB_PORT},
    "user": "${DB_USER}",
    "password": "${DB_PASS}",
    "dbname": "${DB_NAME}",
    "sslmode": "${DB_SSL}"
  }
}
JSON
ok "已生成 config.json"

# go 环境检查
command -v go >/dev/null 2>&1 || die "未检测到 Go，请先安装 Go（>=1.26）：https://go.dev/dl/"
ok "Go: $(go version)"

# 内嵌前端需要 node 出静态产物
if command -v npm >/dev/null 2>&1; then
  info "构建前端静态产物…"
  ( cd web && npm ci && npm run build:static )
  rm -rf server/webui/dist && cp -r web/out server/webui/dist
  info "编译内嵌单二进制…"
  CGO_ENABLED=0 go build -tags embedui -trimpath -o artex ./cmd/artex
else
  warn "未检测到 npm：将编译**不内嵌前端**的后端（前端需另跑 npm run dev）"
  CGO_ENABLED=0 go build -o artex ./cmd/artex
fi
ok "编译完成 → ./artex"

info "启动…（Ctrl-C 退出）"
./artex
